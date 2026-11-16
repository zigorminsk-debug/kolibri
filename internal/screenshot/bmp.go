package screenshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
)

func isBMP(raw []byte) bool {
	return len(raw) >= 54 && raw[0] == 'B' && raw[1] == 'M'
}

// decodeBMP decodes 24-bit and 32-bit uncompressed Windows BMP files (BI_RGB
// and standard BGRA BI_BITFIELDS) without adding external dependencies.
func decodeBMP(raw []byte) (image.Image, error) {
	if len(raw) < 54 {
		return nil, errors.New("повреждённый заголовок BMP")
	}
	pixOffset := int(binary.LittleEndian.Uint32(raw[10:14]))
	dibSize := int(binary.LittleEndian.Uint32(raw[14:18]))
	if dibSize < 40 || len(raw) < 14+dibSize || pixOffset < 14+dibSize || pixOffset >= len(raw) {
		return nil, errors.New("неподдерживаемый заголовок BMP")
	}
	w := int(int32(binary.LittleEndian.Uint32(raw[18:22])))
	hRaw := int(int32(binary.LittleEndian.Uint32(raw[22:26])))
	topDown := hRaw < 0
	h := hRaw
	if topDown {
		h = -hRaw
	}
	if w <= 0 || h <= 0 || w > maxRawDimension || h > maxRawDimension || int64(w)*int64(h) > maxRawPixels {
		return nil, fmt.Errorf("недопустимый размер BMP %d×%d", w, h)
	}
	bpp := int(binary.LittleEndian.Uint16(raw[28:30]))
	comp := binary.LittleEndian.Uint32(raw[30:34])
	if (bpp != 24 && bpp != 32) || (comp != 0 && !(comp == 3 && bpp == 32)) {
		return nil, fmt.Errorf("поддерживаются 24- и 32-битные BMP без сжатия (получено %d бит)", bpp)
	}

	bytesPerPx := bpp / 8
	rowStride := (w*bytesPerPx + 3) &^ 3
	if int64(pixOffset)+int64(rowStride)*int64(h) > int64(len(raw)) {
		return nil, errors.New("данные BMP обрезаны")
	}

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	// Many 32-bit GDI bitmaps leave the alpha channel all zero; treat all-zero
	// alpha as opaque so the screenshot doesn't turn invisible.
	anyAlpha := bpp == 24
	for y := range h {
		srcY := y
		if !topDown {
			srcY = h - 1 - y
		}
		row := raw[pixOffset+srcY*rowStride:]
		for x := range w {
			p := row[x*bytesPerPx:]
			a := uint8(255)
			if bpp == 32 {
				a = p[3]
				if a != 0 {
					anyAlpha = true
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{R: p[2], G: p[1], B: p[0], A: a})
		}
	}
	if !anyAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	return img, nil
}
