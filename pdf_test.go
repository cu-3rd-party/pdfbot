package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"regexp"
	"strconv"
	"testing"
)

func enc(t *testing.T, m image.Image, f string) []byte {
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

func TestToJPEG(t *testing.T) {
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
		{"jpeg как есть", jpg, 30, 20, false, true, false},
		{"серый jpeg как есть", enc(t, gray, "jpeg"), 10, 40, true, true, false},
		{"png перекодируется", enc(t, rgb, "png"), 30, 20, false, false, false},
		{"серый png", enc(t, gray, "png"), 10, 40, true, false, false},
		{"мусор", []byte("hello"), 0, 0, false, false, true},
		{"пусто", nil, 0, 0, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			im, err := toJPEG(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatal("ждали ошибку")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if im.w != tc.w || im.h != tc.h || im.gray != tc.gray {
				t.Fatalf("got %dx%d gray=%v", im.w, im.h, im.gray)
			}
			if tc.same != bytes.Equal(im.jpg, tc.in) {
				t.Fatalf("same=%v", !tc.same)
			}
			if _, err := jpeg.Decode(bytes.NewReader(im.jpg)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// проверяем, что xref указывает ровно на начала объектов и страниц сколько надо
func TestPDF(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 4, 3))
	m.Set(1, 1, color.White)
	im, err := toJPEG(enc(t, m, "png"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pages := range []int{1, 2, 7} {
		var b bytes.Buffer
		p := newPDF(&b)
		for range pages {
			p.addPage(im)
		}
		if err := p.close(); err != nil {
			t.Fatal(err)
		}
		out := b.Bytes()
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
				t.Fatalf("pages=%d: xref объекта %d мимо", pages, i+1)
			}
			want--
		}
		if want != 0 {
			t.Fatalf("pages=%d: в xref не все объекты", pages)
		}
	}
}

func TestCleanName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"pxc_1984", "pxc1984"},
		{"", "user"},
		{"___", "user"},
		{"Вася Пупкин-2", "Вася Пупкин-2"},
		{string(bytes.Repeat([]byte("a"), 100)), string(bytes.Repeat([]byte("a"), 64))},
	}
	for _, tc := range tests {
		if got := cleanName(tc.in); got != tc.want {
			t.Errorf("cleanName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
