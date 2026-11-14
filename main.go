package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// переписал github.com/pxc1984/pdfbot (C#) на go: шлёшь фотки/картинки файлами, /makepdf собирает pdf.
// очередь в postgres, но только file_id телеграма: сами файлы качаем в момент сборки
// и стримим прямо в загрузку pdf, ни на диск, ни целиком в память ничего не ложится.
// так 100 одновременных сборок влезают в 256 МБ (см. pdf.go)

const (
	MAX_QUEUE = 100      // страниц в одном pdf
	MAX_FILE  = 20 << 20 // больше getFile всё равно не отдаёт
	MAX_PDF   = 49 << 20 // боту можно слать документы до 50 МБ
)

// var, чтобы тесты подменяли телеграм
var API = "https://api.telegram.org"

var (
	token   = os.Getenv("TELEGRAM_BOT_TOKEN")
	hookURL = os.Getenv("WEBHOOK_URL")
	addr    = envOr("HTTP_ADDR", ":8080")
	// секрет вебхука выводим из токена, чтобы не заводить ещё одну переменную
	hookSecret = func() string {
		s := sha256.Sum256([]byte("pdfbot-webhook:" + token))
		return hex.EncodeToString(s[:16])
	}()

	db *pgxpool.Pool
	// все запросы в один хост, держим под 100 сборок соединения открытыми
	hc = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100, IdleConnTimeout: time.Minute}}
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type update struct {
	Message *message `json:"message"`
}

type message struct {
	Chat  struct{ ID int64 }         `json:"chat"`
	From  *struct{ Username string } `json:"from"`
	Text  string                     `json:"text"`
	Photo []struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
	} `json:"photo"`
	Document *struct {
		FileID   string `json:"file_id"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"document"`
}

const SCHEMA = `
create table if not exists queue (
	id bigserial primary key,
	chat_id bigint not null,
	file_id text not null,
	file_size bigint not null,
	taken timestamptz, -- не null, пока страница в сборке
	created_at timestamptz not null default now()
);
create index if not exists queue_chat on queue (chat_id, id);
-- реплика одна (Recreate), так что всё взятое в сборку до рестарта - брошено, возвращаем в очередь
update queue set taken = null where taken is not null;`

func main() {
	if token == "" || !strings.HasPrefix(hookURL, "https://") || os.Getenv("DATABASE_URL") == "" {
		log.Fatal("нужны TELEGRAM_BOT_TOKEN, WEBHOOK_URL (https://...) и DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	// ходим через transaction-пулер PgBouncer: соседние запросы могут попасть на разные
	// серверные соединения, поэтому без prepared statements, только simple protocol
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// запросы короткие, сборки держат соединение только на время запроса
	cfg.MaxConns = 10
	if db, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, SCHEMA); err != nil {
		log.Fatal("схема: ", err)
	}

	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	http.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != hookSecret {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var u update
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&u); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// отвечаем сразу, иначе телеграм ретраит апдейт пока мы собираем pdf
		if u.Message != nil {
			go handle(ctx, &u)
		}
	})

	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	err = call(ctx, "setWebhook", url.Values{
		"url":             {hookURL},
		"secret_token":    {hookSecret},
		"allowed_updates": {`["message"]`},
		"max_connections": {"100"},
	}, nil)
	if err != nil {
		log.Fatal("setWebhook: ", err)
	}
	log.Printf("слушаю %s, вебхук %s", addr, hookURL)

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}

func handle(ctx context.Context, u *update) {
	m := u.Message
	chat := m.Chat.ID
	// /cmd@botname в группах
	cmd, _, _ := strings.Cut(strings.TrimSpace(m.Text), "@")

	switch {
	case cmd == "/start":
		send(ctx, chat, "Привет! Я превращаю ваши фотографии в PDF-файл.\n\n"+
			"Просто отправляйте фотографии (лучше файлами, без сжатия).\n"+
			"После отправки:\n/makepdf - собрать все фото в PDF\n/cancel - очистить очередь")
	case cmd == "/makepdf":
		name := "user"
		if m.From != nil {
			name = cleanName(m.From.Username)
		}
		makePDF(ctx, chat, name+"_"+time.Now().UTC().Format("2006-01-02")+".pdf")
	case cmd == "/cancel":
		tag, err := db.Exec(ctx, `delete from queue where chat_id = $1 and taken is null`, chat)
		switch {
		case err != nil:
			log.Print(err)
			send(ctx, chat, "Что-то сломалось, попробуйте ещё раз.")
		case tag.RowsAffected() == 0:
			send(ctx, chat, "Очередь пуста - нечего очищать.")
		default:
			send(ctx, chat, "Всё, забыл.")
		}
	case len(m.Photo) > 0:
		// последний размер самый большой
		p := m.Photo[len(m.Photo)-1]
		enqueue(ctx, chat, p.FileID, p.FileSize, "Фото")
	case m.Document != nil:
		d := m.Document
		if !strings.HasPrefix(d.MimeType, "image/") {
			send(ctx, chat, "Это не картинка, такое в PDF не положу.")
			return
		}
		if d.FileSize > MAX_FILE {
			send(ctx, chat, "Файл больше 20 МБ, телеграм не даст мне его скачать.")
			return
		}
		enqueue(ctx, chat, d.FileID, d.FileSize, "Документ")
	case m.Text != "":
		send(ctx, chat, "Отправьте фото или документ, и я соберу из них PDF.\n\n/makepdf - собрать PDF\n/cancel - отменить")
	}
}

func enqueue(ctx context.Context, chat int64, fileID string, size int64, what string) {
	// вставляем, только если очередь не полная; n - сколько было до вставки
	var n, added int
	err := db.QueryRow(ctx, `
		with c as (select count(*) n from queue where chat_id = $1 and taken is null),
		ins as (insert into queue (chat_id, file_id, file_size) select $1, $2, $3 from c where n < $4 returning 1)
		select n, (select count(*) from ins) from c`, chat, fileID, size, MAX_QUEUE).Scan(&n, &added)
	if err != nil {
		log.Print(err)
		send(ctx, chat, "Не получилось сохранить, пришлите ещё раз.")
		return
	}
	if added == 0 {
		send(ctx, chat, fmt.Sprintf("В очереди уже %d страниц, больше не влезет. /makepdf или /cancel", MAX_QUEUE))
		return
	}
	send(ctx, chat, fmt.Sprintf("%s добавлено! В очереди: %d.\n/makepdf - собрать в PDF\n/cancel - очистить", what, n+1))
}

type page struct {
	id     int64
	fileID string
	size   int64
}

func makePDF(ctx context.Context, chat int64, name string) {
	// берём очередь в сборку; удаляем только после отправки, чтобы при падении не потерять
	rows, err := db.Query(ctx, `update queue set taken = now() where chat_id = $1 and taken is null returning id, file_id, file_size`, chat)
	var q []page
	if err == nil {
		for rows.Next() {
			var p page
			if err = rows.Scan(&p.id, &p.fileID, &p.size); err != nil {
				break
			}
			q = append(q, p)
		}
		rows.Close()
		err = errors.Join(err, rows.Err())
	}
	if err != nil {
		log.Print(err)
		send(ctx, chat, "Что-то сломалось, попробуйте ещё раз.")
		return
	}
	if len(q) == 0 {
		send(ctx, chat, "У вас нет сохранённых фото. Отправьте фотографии, затем /makepdf")
		return
	}
	slices.SortFunc(q, func(a, b page) int { return cmp.Compare(a.id, b.id) })
	ids := make([]int64, len(q))
	var total int64
	for i, p := range q {
		ids[i] = p.id
		total += p.size
	}

	release := func() {
		if _, err := db.Exec(context.Background(), `update queue set taken = null where id = any($1)`, ids); err != nil {
			log.Print(err)
		}
	}
	if total > MAX_PDF {
		release()
		send(ctx, chat, fmt.Sprintf("Файлы весят %d МБ, а телеграм пропускает PDF только до 50 МБ. /cancel и пришлите поменьше.", total>>20))
		return
	}
	send(ctx, chat, "Собираю PDF... Подождите.")

	skipped, err := uploadPDF(ctx, chat, name, q)
	for _, s := range skipped {
		send(ctx, chat, s)
	}
	if err != nil {
		release()
		log.Printf("chat %d: %v", chat, err)
		send(ctx, chat, "Не получилось собрать PDF. Попробуйте /makepdf ещё раз или /cancel.")
		return
	}
	if _, err := db.Exec(context.Background(), `delete from queue where id = any($1)`, ids); err != nil {
		log.Print(err)
	}
}

var errNoPages = errors.New("ни одной страницы")

// uploadPDF собирает pdf прямо в тело sendDocument: страница за страницей из ответов телеграма.
// страницы, которые не картинки, пропускаются (их описания в skipped)
func uploadPDF(ctx context.Context, chat int64, name string, q []page) (skipped []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan struct{})

	go func() {
		defer close(done)
		mw.WriteField("chat_id", fmt.Sprint(chat))
		part, err := mw.CreateFormFile("document", name)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		p := newPDF(part)
		for i, pg := range q {
			if err := addFromTelegram(ctx, p, pg); err != nil {
				if p.err != nil {
					// pdf уже недописан, дальше смысла нет
					pw.CloseWithError(err)
					return
				}
				skipped = append(skipped, fmt.Sprintf("Страницу %d пропустил: %v", i+1, err))
			}
		}
		if len(p.kids) == 0 {
			pw.CloseWithError(errNoPages)
			return
		}
		err = p.close()
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, _ := http.NewRequestWithContext(ctx, "POST", API+"/bot"+token+"/sendDocument", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := hc.Do(req)
	// дожидаемся писателя: после этого skipped уже не меняется
	pr.CloseWithError(io.ErrClosedPipe)
	cancel()
	<-done
	if err != nil {
		return skipped, redact(err)
	}
	defer resp.Body.Close()
	return skipped, decode(resp.Body, nil)
}

func addFromTelegram(ctx context.Context, p *pdfWriter, pg page) error {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := call(ctx, "getFile", url.Values{"file_id": {pg.fileID}}, &f); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", API+"/file/bot"+token+"/"+f.FilePath, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("не скачалось")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("скачивание: HTTP %d", resp.StatusCode)
	}
	size := resp.ContentLength
	if size < 0 {
		size = f.FileSize
	}
	if size <= 0 || size > MAX_FILE {
		return fmt.Errorf("непонятный размер файла %d", size)
	}
	im, err := prepare(resp.Body, size)
	if err != nil {
		return err
	}
	return p.addPage(im)
}

// cleanName: как SanitizeFileName в оригинале - буквы, цифры, пробел и дефис, до 64 символов
func cleanName(s string) string {
	r := []rune(strings.Map(func(c rune) rune {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == ' ' || c == '-' {
			return c
		}
		return -1
	}, s))
	if len(r) == 0 {
		return "user"
	}
	return string(r[:min(len(r), 64)])
}

// --- Bot API руками, библиотека ради четырёх методов не нужна ---

func call(ctx context.Context, method string, v url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", API+"/bot"+token+"/"+method, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	return decode(resp.Body, out)
}

// в ошибках http url с токеном, не светим его в логах
func redact(err error) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

func decode(r io.Reader, out any) error {
	var res struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Description)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Result, out)
}

func send(ctx context.Context, chat int64, text string) {
	if err := call(ctx, "sendMessage", url.Values{"chat_id": {fmt.Sprint(chat)}, "text": {text}}, nil); err != nil {
		log.Printf("sendMessage chat %d: %v", chat, err)
	}
}
