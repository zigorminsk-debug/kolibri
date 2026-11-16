package screenshot

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResizeBMPAndShrink(t *testing.T) {
	// 1. Oversized 4K PNG is resized cleanly and stays below DefaultMaxBytes.
	big := image.NewNRGBA(image.Rect(0, 0, 2400, 1350))
	for y := range 1350 {
		for x := range 2400 {
			big.SetNRGBA(x, y, color.NRGBA{R: uint8((x * 13) & 255), G: uint8((y * 7) & 255), B: 40, A: 255})
		}
	}
	var pbuf bytes.Buffer
	if err := png.Encode(&pbuf, big); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "crash-dialog.png")
	if err := os.WriteFile(path, pbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	im, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if im.Width > DefaultMaxDim || im.Height > DefaultMaxDim {
		t.Fatalf("expected scaled within %d, got %d×%d", DefaultMaxDim, im.Width, im.Height)
	}
	if im.OrigWidth != 2400 || im.OrigHeight != 1350 {
		t.Fatalf("orig dims = %d×%d", im.OrigWidth, im.OrigHeight)
	}
	if len(im.Data) > DefaultMaxBytes {
		t.Fatalf("data %d > %d", len(im.Data), DefaultMaxBytes)
	}
	uri := im.DataURI()
	if !strings.HasPrefix(uri, "data:"+im.MIME+";base64,") || len(uri) != im.Base64Len() {
		t.Fatalf("bad DataURI len=%d want=%d", len(uri), im.Base64Len())
	}

	// Shrink compresses further while preserving OrigWidth/OrigHeight.
	small, err := im.Shrink(640, 48<<10)
	if err != nil {
		t.Fatalf("Shrink: %v", err)
	}
	if small.Width > 640 || len(small.Data) > 48<<10 || small.OrigWidth != 2400 {
		t.Fatalf("unexpected shrunk image: %dx%d %dB orig=%d", small.Width, small.Height, len(small.Data), small.OrigWidth)
	}

	// 2. 32-bit Windows GDI BMP (all-zero alpha) decodes as an opaque PNG.
	bmp := makeBMP32(16, 12, color.NRGBA{R: 220, G: 30, B: 40, A: 0})
	bim, err := Decode("win-err.bmp", bmp)
	if err != nil {
		t.Fatalf("Decode BMP: %v", err)
	}
	if bim.MIME != "image/png" || bim.Width != 16 || bim.Height != 12 {
		t.Fatalf("unexpected BMP result: %+v", bim)
	}
	dec, err := png.Decode(bytes.NewReader(bim.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, a := dec.At(4, 4).RGBA()
	if a != 0xffff || r>>8 != 220 || g>>8 != 30 || b>>8 != 40 {
		t.Fatalf("pixel mismatch rgba=(%d,%d,%d,%d)", r>>8, g>>8, b>>8, a>>8)
	}

	// 3. Clipboard loads a copied image file path straight from textHint.
	clip, err := FromClipboard(context.Background(), path)
	if err != nil || clip.Name != "crash-dialog.png" {
		t.Fatalf("FromClipboard(path): im=%+v err=%v", clip.Name, err)
	}

	// 4. Reject non-image data.
	if _, err := Decode("bad.png", []byte("not an image")); err == nil {
		t.Fatal("expected error for invalid image")
	}
}

func makeBMP32(w, h int, c color.NRGBA) []byte {
	stride := w * 4
	raw := make([]byte, 54+stride*h)
	raw[0], raw[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(raw[2:6], uint32(len(raw)))
	binary.LittleEndian.PutUint32(raw[10:14], 54)
	binary.LittleEndian.PutUint32(raw[14:18], 40)
	binary.LittleEndian.PutUint32(raw[18:22], uint32(w))
	binary.LittleEndian.PutUint32(raw[22:26], uint32(h))
	binary.LittleEndian.PutUint16(raw[26:28], 1)
	binary.LittleEndian.PutUint16(raw[28:30], 32)
	for y := range h {
		for x := range w {
			off := 54 + y*stride + x*4
			raw[off] = c.B
			raw[off+1] = c.G
			raw[off+2] = c.R
			raw[off+3] = c.A
		}
	}
	return raw
}
