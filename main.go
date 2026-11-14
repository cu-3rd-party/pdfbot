package main

import (
	"bufio"
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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// переписал github.com/pxc1984/pdfbot (C#) на go: шлёшь фотки/картинки файлами, /makepdf собирает pdf.
// никакой базы: в очереди храним только file_id телеграма, файлы качаем в момент сборки.
// после рестарта очереди теряются - не страшно, пользователь просто перешлёт фотки

const (
	MAX_QUEUE  = 100      // страниц в одном pdf
	MAX_FILE   = 20 << 20 // больше getFile всё равно не отдаёт
	MAX_BUILDS = 2        // одновременных сборок, чтобы влезть в 256 МБ
	API        = "https://api.telegram.org"
)

var (
	token   = os.Getenv("TELEGRAM_BOT_TOKEN")
	hookURL = os.Getenv("WEBHOOK_URL")
	addr    = envOr("HTTP_ADDR", ":8080")
	// секрет вебхука выводим из токена, чтобы не заводить ещё одну переменную
	hookSecret = func() string {
		s := sha256.Sum256([]byte("pdfbot-webhook:" + token))
		return hex.EncodeToString(s[:16])
	}()

	mu     sync.Mutex
	queues = map[int64][]string{}
	builds = make(chan struct{}, MAX_BUILDS)
	hc     = &http.Client{Timeout: 2 * time.Minute}
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type update struct {
	Message *struct {
		Chat  struct{ ID int64 }                    `json:"chat"`
		From  *struct{ Username, FirstName string } `json:"from"`
		Text  string                                `json:"text"`
		Photo []struct {
			FileID   string `json:"file_id"`
			FileSize int    `json:"file_size"`
		} `json:"photo"`
		Document *struct {
			FileID   string `json:"file_id"`
			MimeType string `json:"mime_type"`
			FileSize int    `json:"file_size"`
		} `json:"document"`
	} `json:"message"`
}

func main() {
	if token == "" || !strings.HasPrefix(hookURL, "https://") {
		log.Fatal("нужны TELEGRAM_BOT_TOKEN и WEBHOOK_URL (https://...)")
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
			go handle(&u)
		}
	})

	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	err := call("setWebhook", url.Values{
		"url":             {hookURL},
		"secret_token":    {hookSecret},
		"allowed_updates": {`["message"]`},
		"max_connections": {"10"},
	}, nil)
	if err != nil {
		log.Fatal("setWebhook: ", err)
	}
	log.Printf("слушаю %s, вебхук %s", addr, hookURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}

func handle(u *update) {
	m := u.Message
	chat := m.Chat.ID
	// /cmd@botname в группах
	cmd, _, _ := strings.Cut(strings.TrimSpace(m.Text), "@")

	switch {
	case cmd == "/start":
		send(chat, "Привет! Я превращаю ваши фотографии в PDF-файл.\n\n"+
			"Просто отправляйте фотографии (лучше файлами, без сжатия).\n"+
			"После отправки:\n/makepdf - собрать все фото в PDF\n/cancel - очистить очередь")
	case cmd == "/makepdf":
		makePDF(chat, m.From)
	case cmd == "/cancel":
		mu.Lock()
		n := len(queues[chat])
		delete(queues, chat)
		mu.Unlock()
		if n == 0 {
			send(chat, "Очередь пуста - нечего очищать.")
			return
		}
		send(chat, "Всё, забыл.")
	case len(m.Photo) > 0:
		// последний размер самый большой
		enqueue(chat, m.Photo[len(m.Photo)-1].FileID, "Фото")
	case m.Document != nil:
		d := m.Document
		if !strings.HasPrefix(d.MimeType, "image/") {
			send(chat, "Это не картинка, такое в PDF не положу.")
			return
		}
		if d.FileSize > MAX_FILE {
			send(chat, "Файл больше 20 МБ, телеграм не даст мне его скачать.")
			return
		}
		enqueue(chat, d.FileID, "Документ")
	case m.Text != "":
		send(chat, "Отправьте фото или документ, и я соберу из них PDF.\n\n/makepdf - собрать PDF\n/cancel - отменить")
	}
}

func enqueue(chat int64, fileID, what string) {
	mu.Lock()
	q := queues[chat]
	full := len(q) >= MAX_QUEUE
	if !full {
		q = append(q, fileID)
		queues[chat] = q
	}
	mu.Unlock()
	if full {
		send(chat, fmt.Sprintf("В очереди уже %d страниц, больше не влезет. /makepdf или /cancel", MAX_QUEUE))
		return
	}
	send(chat, fmt.Sprintf("%s добавлено! В очереди: %d.\n/makepdf - собрать в PDF\n/cancel - очистить", what, len(q)))
}

func makePDF(chat int64, from *struct{ Username, FirstName string }) {
	mu.Lock()
	q := queues[chat]
	delete(queues, chat)
	mu.Unlock()
	if len(q) == 0 {
		send(chat, "У вас нет сохранённых фото. Отправьте фотографии, затем /makepdf")
		return
	}
	send(chat, "Собираю PDF... Подождите.")

	builds <- struct{}{}
	defer func() { <-builds }()

	// pdf пишем во временный файл, а не в память: 100 страниц по паре мегабайт
	f, err := os.CreateTemp("", "pdfbot-*.pdf")
	if err != nil {
		log.Print(err)
		send(chat, "Не получилось собрать PDF, попробуйте ещё раз.")
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()

	bw := bufio.NewWriter(f)
	p := newPDF(bw)
	for i, id := range q {
		b, err := download(id)
		if err == nil {
			var im pdfImg
			im, err = toJPEG(b)
			if err == nil {
				p.addPage(im)
			}
		}
		if err != nil {
			log.Printf("chat %d, страница %d: %v", chat, i+1, err)
			send(chat, fmt.Sprintf("Страницу %d пропустил: %v", i+1, err))
		}
	}
	if len(p.kids) == 0 {
		send(chat, "Ни одной страницы собрать не вышло.")
		return
	}
	if err := p.close(); err != nil {
		log.Print(err)
		send(chat, "Не получилось собрать PDF, попробуйте ещё раз.")
		return
	}
	if err := bw.Flush(); err != nil {
		log.Print(err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		log.Print(err)
		return
	}

	name := "user"
	if from != nil {
		name = cleanName(from.Username)
	}
	name += "_" + time.Now().UTC().Format("2006-01-02") + ".pdf"
	if err := sendDocument(chat, name, f); err != nil {
		log.Printf("sendDocument chat %d: %v", chat, err)
		send(chat, "Не получилось отправить PDF.")
	}
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

// --- Bot API руками, библиотека ради пяти методов не нужна ---

func call(method string, v url.Values, out any) error {
	resp, err := hc.PostForm(API+"/bot"+token+"/"+method, v)
	if err != nil {
		// в ошибке url с токеном, не светим его в логах
		return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
	}
	defer resp.Body.Close()
	return decode(resp.Body, out)
}

func decode(r io.Reader, out any) error {
	var res struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(r).Decode(&res); err != nil {
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

func send(chat int64, text string) {
	err := call("sendMessage", url.Values{"chat_id": {fmt.Sprint(chat)}, "text": {text}}, nil)
	if err != nil {
		log.Printf("sendMessage chat %d: %v", chat, err)
	}
}

func download(fileID string) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := call("getFile", url.Values{"file_id": {fileID}}, &f); err != nil {
		return nil, err
	}
	resp, err := hc.Get(API + "/file/bot" + token + "/" + f.FilePath)
	if err != nil {
		return nil, errors.New("не скачалось")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("скачивание: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MAX_FILE+1))
}

// sendDocument стримит файл через pipe, целиком в память не читаем
func sendDocument(chat int64, name string, r io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		mw.WriteField("chat_id", fmt.Sprint(chat))
		part, err := mw.CreateFormFile("document", name)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := hc.Post(API+"/bot"+token+"/sendDocument", mw.FormDataContentType(), pr)
	if err != nil {
		pr.CloseWithError(err)
		return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
	}
	defer resp.Body.Close()
	return decode(resp.Body, nil)
}
