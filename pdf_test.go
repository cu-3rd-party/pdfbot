package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func enc(t testing.TB, m image.Image, f string) []byte {
	var b bytes.Buffer
	var err error
	if f == "png" {
		err = png.Encode(&b, m)
	} else {
		err = jpeg.Encode(&b, m, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPrepare(t *testing.T) {
	rgb := image.NewRGBA(image.Rect(0, 0, 30, 20))
	gray := image.NewGray(image.Rect(0, 0, 10, 40))
	jpg := enc(t, rgb, "jpeg")
	tests := []struct {
		name       string
		in         []byte
		w, h       int
		gray, same bool
		bad        bool
	}{
		{"jpeg стримится как есть", jpg, 30, 20, false, true, false},
		{"серый jpeg как есть", enc(t, gray, "jpeg"), 10, 40, true, true, false},
		{"png перекодируется", enc(t, rgb, "png"), 30, 20, false, false, false},
		{"серый png", enc(t, gray, "png"), 10, 40, true, false, false},
		{"огромный png", enc(t, image.NewGray(image.Rect(0, 0, 5000, 4000)), "png"), 0, 0, false, false, true},
		{"мусор", []byte("hello"), 0, 0, false, false, true},
		{"пусто", nil, 0, 0, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			im, err := prepare(bytes.NewReader(tc.in), int64(len(tc.in)))
			if tc.bad {
				if err == nil {
					t.Fatal("ждали ошибку")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(im.body)
			if im.w != tc.w || im.h != tc.h || im.gray != tc.gray || int64(len(body)) != im.size {
				t.Fatalf("got %dx%d gray=%v size=%d/%d", im.w, im.h, im.gray, im.size, len(body))
			}
			if tc.same != bytes.Equal(body, tc.in) {
				t.Fatalf("same=%v", !tc.same)
			}
			if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// checkPDF: заголовок, хвост, число страниц и что xref указывает ровно на начала объектов
func checkPDF(t testing.TB, out []byte, pages int) {
	t.Helper()
	if !bytes.HasPrefix(out, []byte("%PDF-1.4")) || !bytes.HasSuffix(out, []byte("%%EOF\n")) {
		t.Fatal("нет заголовка/хвоста")
	}
	if n := bytes.Count(out, []byte("/Type /Page ")); n != pages {
		t.Fatalf("страниц %d, ждали %d", n, pages)
	}
	want := 2 + 3*pages
	for i, mm := range regexp.MustCompile(`(\d{10}) 00000 n`).FindAllSubmatch(out, -1) {
		off, _ := strconv.Atoi(string(mm[1]))
		if !bytes.HasPrefix(out[off:], []byte(strconv.Itoa(i+1)+" 0 obj")) {
			t.Fatalf("xref объекта %d мимо", i+1)
		}
		want--
	}
	if want != 0 {
		t.Fatal("в xref не все объекты")
	}
}

func TestPDF(t *testing.T) {
	jpg := enc(t, image.NewRGBA(image.Rect(0, 0, 4, 3)), "jpeg")
	for _, pages := range []int{1, 2, 7} {
		var b bytes.Buffer
		p := newPDF(&b)
		for range pages {
			im, err := prepare(bytes.NewReader(jpg), int64(len(jpg)))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.addPage(im); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.close(); err != nil {
			t.Fatal(err)
		}
		checkPDF(t, b.Bytes(), pages)
	}
}

func TestShortBody(t *testing.T) {
	jpg := enc(t, image.NewRGBA(image.Rect(0, 0, 4, 3)), "jpeg")
	im, _ := prepare(bytes.NewReader(jpg), int64(len(jpg))+10)
	if err := newPDF(io.Discard).addPage(im); err == nil {
		t.Fatal("недокачанная картинка должна ломать pdf")
	}
}

func TestCleanName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"pxc_1984", "pxc1984"},
		{"", "user"},
		{"___", "user"},
		{"Вася Пупкин-2", "Вася Пупкин-2"},
		{strings.Repeat("a", 100), strings.Repeat("a", 64)},
	}
	for _, tc := range tests {
		if got := cleanName(tc.in); got != tc.want {
			t.Errorf("cleanName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// фейковый телеграм: getFile/file отдают картинки, sendDocument принимает pdf и считает байты.
// files: file_id -> содержимое
func fakeTelegram(t testing.TB, files map[string][]byte, keep bool, onPDF func(n int64, pdf []byte)) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			r.ParseForm()
			id := r.Form.Get("file_id")
			fmt.Fprintf(w, `{"ok":true,"result":{"file_path":"f/%s","file_size":%d}}`, id, len(files[id]))
		case strings.Contains(r.URL.Path, "/file/"):
			b, ok := files[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			// как телеграм, отдаём кусками
			for len(b) > 0 {
				k := min(len(b), 32<<10)
				w.Write(b[:k])
				b = b[k:]
			}
		case strings.HasSuffix(r.URL.Path, "/sendDocument"):
			mr, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			// в нагрузочном тесте pdf не храним, чтобы мерить только память бота
			var pdf bytes.Buffer
			var n int64
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FormName() != "document" {
					continue
				}
				if keep {
					n, _ = io.Copy(&pdf, part)
				} else {
					n, _ = io.Copy(io.Discard, part)
				}
			}
			onPDF(n, pdf.Bytes())
			io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	API = srv.URL
}

func TestUpload(t *testing.T) {
	jpg := enc(t, image.NewRGBA(image.Rect(0, 0, 8, 6)), "jpeg")
	files := map[string][]byte{"a": jpg, "b": enc(t, image.NewGray(image.Rect(0, 0, 3, 3)), "png"), "txt": []byte("не картинка")}
	var got []byte
	fakeTelegram(t, files, true, func(_ int64, pdf []byte) { got = pdf })

	tests := []struct {
		name    string
		ids     []string
		pages   int
		skipped int
		err     bool
	}{
		{"две страницы", []string{"a", "b"}, 2, 0, false},
		{"мусор пропускается", []string{"a", "txt", "a"}, 2, 1, false},
		{"нет такого файла", []string{"nope", "a"}, 1, 1, false},
		{"одни ошибки - не шлём", []string{"txt"}, 0, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			var q []page
			for _, id := range tc.ids {
				q = append(q, page{fileID: id})
			}
			skipped, err := uploadPDF(context.Background(), 1, "x.pdf", q)
			if (err != nil) != tc.err || len(skipped) != tc.skipped {
				t.Fatalf("err=%v skipped=%v", err, skipped)
			}
			if !tc.err {
				checkPDF(t, got, tc.pages)
			}
		})
	}
}

// 100 почти одновременных сборок по 20 фоток ~1 МБ: куча не должна расти от числа сборок
func TestHundredBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	// шум, чтобы jpeg не сжался в ноль
	m := image.NewRGBA(image.Rect(0, 0, 1200, 900))
	for i := range m.Pix {
		m.Pix[i] = byte(i * 7919 >> 3)
	}
	m.Set(0, 0, color.White)
	photo := enc(t, m, "jpeg")
	files := map[string][]byte{"p": photo}
	var sent atomic.Int64
	fakeTelegram(t, files, false, func(n int64, _ []byte) { sent.Add(n) })

	q := make([]page, 20)
	for i := range q {
		q[i] = page{fileID: "p"}
	}
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	stop, sampled := make(chan struct{}), make(chan struct{})
	var peak uint64
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapInuse)
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			if _, err := uploadPDF(context.Background(), int64(i), "x.pdf", q); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(stop)
	<-sampled
	// куча общая с фейковым сервером, так что это оценка сверху
	t.Logf("jpeg %d КБ, отправлено %d МБ, пик кучи %d МБ (база %d МБ)", len(photo)>>10, sent.Load()>>20, peak>>20, base.HeapInuse>>20)
}
