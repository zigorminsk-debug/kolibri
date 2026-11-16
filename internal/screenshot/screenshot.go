// Package screenshot loads, resizes and encodes user screenshots (for instance
// a crash dialog or stack trace from a program under test) so they fit cleanly
// inside colibri's multimodal chat API, and provides clipboard capture plus a
// local OCR fallback for text-only models.
package screenshot

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxInputBytes bounds the raw file read from disk or clipboard.
	MaxInputBytes = 32 << 20
	// DefaultMaxDim caps the longer edge while keeping small dialog and stack
	// trace fonts crisp.
	DefaultMaxDim = 1600
	// DefaultMaxBytes caps the encoded image size (~1.2 MiB as base64) so the
	// whole chat request stays comfortably below colibri's 4 MiB body limit.
	DefaultMaxBytes = 900 << 10
	// MaxAttached limits how many screenshots a single task carries.
	MaxAttached = 6

	maxRawDimension = 16384
	maxRawPixels    = 80_000_000
)

// Extensions lists file extensions recognised by the file picker and drag-and-drop.
var Extensions = []string{".png", ".jpg", ".jpeg", ".bmp", ".gif"}

// ErrNoClipboardImage reports that the system clipboard holds no screenshot.
var ErrNoClipboardImage = errors.New("в буфере обмена нет изображения (сделайте снимок экрана Win+Shift+S или PrintScreen и попробуйте снова)")

// ErrNoOCR reports that no local OCR tool is available on this machine.
var ErrNoOCR = errors.New("локальное распознавание текста (OCR) недоступно")

// Image is one prepared screenshot ready to be sent to colibri or previewed.
type Image struct {
	Name       string
	MIME       string
	Data       []byte
	Width      int
	Height     int
	OrigWidth  int
	OrigHeight int
}

// DataURI formats the image as the base64 data: URI colibri expects in
// `image_url.url`.
func (im Image) DataURI() string {
	mime := im.MIME
	if mime == "" {
		mime = "image/png"
	}
	var b strings.Builder
	b.Grow(len("data:;base64,") + len(mime) + base64.StdEncoding.EncodedLen(len(im.Data)))
	b.WriteString("data:")
	b.WriteString(mime)
	b.WriteString(";base64,")
	enc := base64.NewEncoder(base64.StdEncoding, &b)
	_, _ = enc.Write(im.Data)
	_ = enc.Close()
	return b.String()
}

// Base64Len returns the exact byte length of DataURI without allocating it.
func (im Image) Base64Len() int {
	mime := im.MIME
	if mime == "" {
		mime = "image/png"
	}
	return len("data:;base64,") + len(mime) + base64.StdEncoding.EncodedLen(len(im.Data))
}

// Label is a short caption for UI chips and logs, e.g. "crash.png (1280×720)".
func (im Image) Label() string {
	name := strings.TrimSpace(im.Name)
	if name == "" {
		name = "screenshot.png"
	}
	w, h := im.OrigWidth, im.OrigHeight
	if w <= 0 || h <= 0 {
		w, h = im.Width, im.Height
	}
	if w > 0 && h > 0 {
		return fmt.Sprintf("%s (%d×%d)", name, w, h)
	}
	return name
}

// Shrink produces a smaller copy when a request nears colibri's 4 MiB body cap
// or runs into a tight model context.
func (im Image) Shrink(maxDim, maxBytes int) (Image, error) {
	if len(im.Data) == 0 {
		return Image{}, errors.New("пустое изображение")
	}
	out, err := DecodeWithLimit(im.Name, im.Data, maxDim, maxBytes)
	if err != nil {
		return Image{}, err
	}
	if im.OrigWidth > 0 && im.OrigHeight > 0 {
		out.OrigWidth, out.OrigHeight = im.OrigWidth, im.OrigHeight
	}
	return out, nil
}

// IsImagePath reports whether path has a supported screenshot extension.
func IsImagePath(path string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	for _, want := range Extensions {
		if ext == want {
			return true
		}
	}
	return false
}

// LoadFile reads a screenshot file from disk and prepares it for colibri.
func LoadFile(path string) (Image, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Image{}, errors.New("не указан путь к скриншоту")
	}
	info, err := os.Stat(path)
	if err != nil {
		return Image{}, fmt.Errorf("не удалось открыть скриншот %s: %w", filepath.Base(path), err)
	}
	if info.IsDir() {
		return Image{}, fmt.Errorf("%s — это папка, а не изображение", filepath.Base(path))
	}
	if info.Size() == 0 {
		return Image{}, fmt.Errorf("файл %s пуст", filepath.Base(path))
	}
	if info.Size() > MaxInputBytes {
		return Image{}, fmt.Errorf("файл %s слишком большой (%d МБ, максимум %d МБ)",
			filepath.Base(path), info.Size()>>20, MaxInputBytes>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return Image{}, fmt.Errorf("не удалось прочитать %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxInputBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("ошибка чтения %s: %w", filepath.Base(path), err)
	}
	if len(data) > MaxInputBytes {
		return Image{}, fmt.Errorf("файл %s превышает %d МБ", filepath.Base(path), MaxInputBytes>>20)
	}
	return Decode(filepath.Base(path), data)
}

// Decode validates and prepares raw image bytes (PNG, JPEG, GIF or BMP).
func Decode(name string, raw []byte) (Image, error) {
	return DecodeWithLimit(name, raw, DefaultMaxDim, DefaultMaxBytes)
}

// DecodeWithLimit validates raw image bytes and scales/compresses the image so
// its longer edge fits maxDim and its encoded representation fits maxBytes.
func DecodeWithLimit(name string, raw []byte, maxDim, maxBytes int) (Image, error) {
	if len(raw) == 0 {
		return Image{}, errors.New("пустые данные изображения")
	}
	if maxDim < 128 {
		maxDim = DefaultMaxDim
	}
	if maxBytes < 16<<10 {
		maxBytes = DefaultMaxBytes
	}
	name = cleanImageName(name)

	src, format, err := decodeSafe(raw)
	if err != nil {
		return Image{}, fmt.Errorf("не удалось распознать изображение %s (поддерживаются PNG, JPEG, BMP и GIF): %w", name, err)
	}
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	if origW <= 0 || origH <= 0 {
		return Image{}, fmt.Errorf("изображение %s имеет нулевой размер", name)
	}

	// Allow slightly more vertical/horizontal headroom for tall stack traces
	// while keeping total pixel count modest.
	limit := maxDim
	if (origH > origW*2 || origW > origH*2) && maxDim == DefaultMaxDim {
		limit = 2048
	}

	// Fast path: already a compact PNG or JPEG within bounds.
	if origW <= limit && origH <= limit && len(raw) <= maxBytes && (format == "png" || format == "jpeg") {
		return Image{
			Name:       name,
			MIME:       "image/" + format,
			Data:       bytes.Clone(raw),
			Width:      origW,
			Height:     origH,
			OrigWidth:  origW,
			OrigHeight: origH,
		}, nil
	}

	scaled := fitWithin(src, limit)
	sw, sh := scaled.Bounds().Dx(), scaled.Bounds().Dy()

	// Prefer lossless PNG first: UI dialogs, terminal output and error text
	// compress tightly and stay razor-sharp without JPEG ringing.
	if format != "jpeg" {
		if pngData, perr := encodePNG(scaled); perr == nil && len(pngData) <= maxBytes {
			return Image{
				Name:       name,
				MIME:       "image/png",
				Data:       pngData,
				Width:      sw,
				Height:     sh,
				OrigWidth:  origW,
				OrigHeight: origH,
			}, nil
		}
	}

	// Fall back to progressively smaller high-quality JPEGs until within budget.
	cur := scaled
	for step := range 4 {
		if step > 0 {
			nextMax := max(320, max(cur.Bounds().Dx(), cur.Bounds().Dy())*3/4)
			cur = fitWithin(cur, nextMax)
		}
		flat := flattenOpaque(cur)
		for _, q := range []int{88, 78, 66} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: q}); err == nil && buf.Len() <= maxBytes {
				return Image{
					Name:       name,
					MIME:       "image/jpeg",
					Data:       buf.Bytes(),
					Width:      cur.Bounds().Dx(),
					Height:     cur.Bounds().Dy(),
					OrigWidth:  origW,
					OrigHeight: origH,
				}, nil
			}
		}
	}
	return Image{}, fmt.Errorf("не удалось ужать скриншот %s до %d КБ", name, maxBytes>>10)
}

func decodeSafe(raw []byte) (image.Image, string, error) {
	if isBMP(raw) {
		img, err := decodeBMP(raw)
		return img, "bmp", err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxRawDimension || cfg.Height > maxRawDimension ||
		int64(cfg.Width)*int64(cfg.Height) > maxRawPixels {
		return nil, "", fmt.Errorf("слишком большое разрешение %d×%d", cfg.Width, cfg.Height)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	return img, format, err
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// flattenOpaque composites translucent pixels onto white so JPEG encoding never
// turns transparent window shadows into solid black blocks covering text.
func flattenOpaque(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// fitWithin scales src down with area-averaging when either edge exceeds
// maxEdge. Area-averaging keeps thin font strokes readable in stack traces.
func fitWithin(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxEdge && sh <= maxEdge {
		return src
	}
	dw, dh := sw, sh
	if sw >= sh {
		dw = maxEdge
		dh = max(1, (sh*maxEdge+sw/2)/sw)
	} else {
		dw = max(1, (sw*maxEdge+sh/2)/sh)
		dh = maxEdge
	}

	rgba := image.NewNRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for dy := range dh {
		sy0 := dy * sh / dh
		sy1 := max(sy0+1, (dy+1)*sh/dh)
		if sy1 > sh {
			sy1 = sh
		}
		for dx := range dw {
			sx0 := dx * sw / dw
			sx1 := max(sx0+1, (dx+1)*sw/dw)
			if sx1 > sw {
				sx1 = sw
			}
			var rSum, gSum, bSum, aSum, cnt uint64
			for sy := sy0; sy < sy1; sy++ {
				row := sy * rgba.Stride
				for sx := sx0; sx < sx1; sx++ {
					off := row + sx*4
					a := uint64(rgba.Pix[off+3])
					rSum += uint64(rgba.Pix[off]) * a
					gSum += uint64(rgba.Pix[off+1]) * a
					bSum += uint64(rgba.Pix[off+2]) * a
					aSum += a
					cnt++
				}
			}
			doff := dy*dst.Stride + dx*4
			if aSum == 0 || cnt == 0 {
				continue
			}
			dst.Pix[doff] = uint8((rSum + aSum/2) / aSum)
			dst.Pix[doff+1] = uint8((gSum + aSum/2) / aSum)
			dst.Pix[doff+2] = uint8((bSum + aSum/2) / aSum)
			dst.Pix[doff+3] = uint8((aSum + cnt/2) / cnt)
		}
	}
	return dst
}

func cleanImageName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." || name == "/" || name == `\` {
		return "screenshot.png"
	}
	return name
}
