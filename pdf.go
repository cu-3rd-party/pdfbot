package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
)

// сам пишу PDF: картинки кладём как JPEG (DCTDecode), библиотека тут не нужна.
// одна страница = одна картинка, 1 пиксель = 1 pt, вьюеры всё равно масштабируют

// картинки больше этого не декодируем: RGBA на 25 Мп это уже 100 МБ, а лимит пода 256
const MAX_PIXELS = 25_000_000

type pdfImg struct {
	w, h int
	gray bool
	jpg  []byte
}

// toJPEG: JPEG отдаём как есть, остальное (png, gif, cmyk-jpeg) перекодируем
func toJPEG(b []byte) (pdfImg, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return pdfImg{}, fmt.Errorf("не картинка: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MAX_PIXELS {
		return pdfImg{}, fmt.Errorf("картинка %dx%d слишком большая", cfg.Width, cfg.Height)
	}
	gray := cfg.ColorModel == color.GrayModel
	if format == "jpeg" && (gray || cfg.ColorModel == color.YCbCrModel) {
		return pdfImg{cfg.Width, cfg.Height, gray, b}, nil
	}
	m, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return pdfImg{}, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, m, &jpeg.Options{Quality: 90}); err != nil {
		return pdfImg{}, err
	}
	// jpeg.Encode серое пишет серым, остальное в YCbCr
	_, isGray := m.(*image.Gray)
	return pdfImg{cfg.Width, cfg.Height, isGray, out.Bytes()}, nil
}

// pdfWriter пишет потоково: страницы добавляются по одной, в памяти только оффсеты
type pdfWriter struct {
	w    io.Writer
	n    int64
	offs []int64 // offs[i] = оффсет объекта i+1
	kids []int
	err  error
}

func (p *pdfWriter) printf(f string, a ...any) {
	if p.err != nil {
		return
	}
	k, err := fmt.Fprintf(p.w, f, a...)
	p.n += int64(k)
	p.err = err
}

func (p *pdfWriter) write(b []byte) {
	if p.err != nil {
		return
	}
	k, err := p.w.Write(b)
	p.n += int64(k)
	p.err = err
}

// obj начинает объект с номером id (номера раздаём заранее, порядок записи любой)
func (p *pdfWriter) obj(id int) {
	for len(p.offs) < id {
		p.offs = append(p.offs, 0)
	}
	p.offs[id-1] = p.n
	p.printf("%d 0 obj\n", id)
}

// 1 - каталог, 2 - дерево страниц (пишем в конце, когда знаем kids)
func newPDF(w io.Writer) *pdfWriter {
	p := &pdfWriter{w: w, offs: []int64{0, 0}}
	p.printf("%%PDF-1.4\n%%\xe2\xe3\xcf\xd3\n")
	return p
}

func (p *pdfWriter) addPage(im pdfImg) {
	// page, image, contents
	pg := len(p.offs) + 1
	img, cnt := pg+1, pg+2
	p.kids = append(p.kids, pg)

	p.obj(pg)
	p.printf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>\nendobj\n", im.w, im.h, img, cnt)

	cs := "/DeviceRGB"
	if im.gray {
		cs = "/DeviceGray"
	}
	p.obj(img)
	p.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", im.w, im.h, cs, len(im.jpg))
	p.write(im.jpg)
	p.printf("\nendstream\nendobj\n")

	s := fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q", im.w, im.h)
	p.obj(cnt)
	p.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(s), s)
}

func (p *pdfWriter) close() error {
	p.obj(1)
	p.printf("<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	p.obj(2)
	p.printf("<< /Type /Pages /Count %d /Kids [", len(p.kids))
	for _, k := range p.kids {
		p.printf("%d 0 R ", k)
	}
	p.printf("] >>\nendobj\n")

	xref := p.n
	p.printf("xref\n0 %d\n0000000000 65535 f \n", len(p.offs)+1)
	for _, o := range p.offs {
		p.printf("%010d 00000 n \n", o)
	}
	p.printf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(p.offs)+1, xref)
	return p.err
}
