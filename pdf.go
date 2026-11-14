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
	"runtime/debug"
)

// сам пишу PDF: картинки кладём как JPEG (DCTDecode), библиотека тут не нужна.
// одна страница = одна картинка, 1 пиксель = 1 pt, вьюеры всё равно масштабируют.
//
// по памяти: JPEG (а фото из телеги всегда JPEG) не декодируем, а стримим как есть из
// ответа телеграма прямо в pdf - на сборку уходит только буфер заголовка.
// png/gif приходится декодировать целиком, поэтому такие страницы по одной на весь процесс

const (
	// png больше этого не декодируем: RGBA на 16 Мп это 64 МБ
	MAX_PIXELS = 16_000_000
	// заголовок, по которому DecodeConfig находит размер (EXIF бывает до 64 КБ)
	MAX_HEADER = 256 << 10
)

// декодировать (не JPEG) можно только одну картинку за раз
var decodeSem = make(chan struct{}, 1)

type pdfImg struct {
	w, h int
	gray bool
	size int64     // длина JPEG
	body io.Reader // сам JPEG
}

// prepare читает заголовок картинки длиной size. JPEG отдаёт потоком без декодирования,
// остальное перекодирует в JPEG в память
func prepare(r io.Reader, size int64) (pdfImg, error) {
	var head bytes.Buffer
	cfg, format, err := image.DecodeConfig(io.TeeReader(io.LimitReader(r, MAX_HEADER), &head))
	if err != nil {
		return pdfImg{}, fmt.Errorf("не картинка: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return pdfImg{}, fmt.Errorf("кривой размер %dx%d", cfg.Width, cfg.Height)
	}
	full := io.MultiReader(&head, r)
	gray := cfg.ColorModel == color.GrayModel
	if format == "jpeg" && (gray || cfg.ColorModel == color.YCbCrModel) {
		return pdfImg{cfg.Width, cfg.Height, gray, size, full}, nil
	}
	if cfg.Width*cfg.Height > MAX_PIXELS {
		return pdfImg{}, fmt.Errorf("%s %dx%d слишком большая, пришлите JPEG", format, cfg.Width, cfg.Height)
	}

	decodeSem <- struct{}{}
	defer func() {
		<-decodeSem
		// декодированная картинка - десятки МБ, сразу отдаём их ОС, а не ждём GC
		debug.FreeOSMemory()
	}()
	m, _, err := image.Decode(full)
	if err != nil {
		return pdfImg{}, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, m, &jpeg.Options{Quality: 90}); err != nil {
		return pdfImg{}, err
	}
	// jpeg.Encode серое пишет серым, остальное в YCbCr
	_, isGray := m.(*image.Gray)
	return pdfImg{cfg.Width, cfg.Height, isGray, int64(out.Len()), &out}, nil
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

// addPage: если тело короче/длиннее заявленного, pdf уже битый - ошибка фатальная
func (p *pdfWriter) addPage(im pdfImg) error {
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
	p.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", im.w, im.h, cs, im.size)
	if p.err == nil {
		k, err := io.Copy(p.w, io.LimitReader(im.body, im.size))
		p.n += k
		p.err = err
		if err == nil && k != im.size {
			p.err = fmt.Errorf("картинка %d байт вместо %d", k, im.size)
		}
	}
	p.printf("\nendstream\nendobj\n")

	s := fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q", im.w, im.h)
	p.obj(cnt)
	p.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(s), s)
	return p.err
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
