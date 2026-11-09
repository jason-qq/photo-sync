package preview

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRaw builds a minimal little-endian TIFF with an Orientation tag in IFD0,
// followed by junk containing false SOI markers, a small decoy JPEG, and a
// larger JPEG of the given size — roughly the shape of an ARW/NEF.
func fakeRaw(t *testing.T, orientation, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("II")
	binary.Write(&b, le, uint16(42))
	binary.Write(&b, le, uint32(8))           // IFD0 offset
	binary.Write(&b, le, uint16(1))           // 1 entry
	binary.Write(&b, le, []uint16{0x0112, 3}) // Orientation, SHORT
	binary.Write(&b, le, uint32(1))           // count
	binary.Write(&b, le, []uint16{uint16(orientation), 0})
	binary.Write(&b, le, uint32(0)) // no next IFD

	b.Write(bytes.Repeat([]byte{0xAB, 0xFF, 0xD8, 0xFF, 0x00}, 200))
	mustEncode(t, &b, solid(160, 120))
	mustEncode(t, &b, gradient(w, h))
	b.Write(make([]byte, 4096))
	return b.Bytes()
}

func solid(w, h int) image.Image {
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

// gradient's top-left pixel is bright and the rest dark, so orientation
// changes are observable.
func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := uint8(20)
			if x < w/4 && y < h/4 {
				c = 240
			}
			img.Set(x, y, color.RGBA{c, c, c, 255})
		}
	}
	return img
}

func mustEncode(t *testing.T, b *bytes.Buffer, img image.Image) {
	t.Helper()
	if err := jpeg.Encode(b, img, nil); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid JPEG: %v", err)
	}
	return img
}

func size(img image.Image) (int, int) { return img.Bounds().Dx(), img.Bounds().Dy() }

func TestGenerateSizes(t *testing.T) {
	tests := []struct {
		name               string
		orientation, w, h  int
		thumbW, thumbH     int
		previewW, previewH int
	}{
		{"landscape", 1, 3000, 2000, 400, 266, 2048, 1365},
		{"portrait via orientation 6", 6, 3000, 2000, 266, 400, 1365, 2048},
		{"portrait via orientation 8", 8, 3000, 2000, 266, 400, 1365, 2048},
		{"small source not upscaled", 1, 1616, 1080, 400, 267, 1616, 1080},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thumb, prev, err := Generate(fakeRaw(t, tt.orientation, tt.w, tt.h))
			if err != nil {
				t.Fatal(err)
			}
			if w, h := size(decode(t, thumb)); w != tt.thumbW || h != tt.thumbH {
				t.Errorf("thumb = %dx%d, want %dx%d", w, h, tt.thumbW, tt.thumbH)
			}
			if w, h := size(decode(t, prev)); w != tt.previewW || h != tt.previewH {
				t.Errorf("preview = %dx%d, want %dx%d", w, h, tt.previewW, tt.previewH)
			}
		})
	}
}

func TestGenerateOrientation(t *testing.T) {
	// After orientation 6 (rotate 90° clockwise), the bright top-left block
	// of the source moves to the top-right.
	_, prev, err := Generate(fakeRaw(t, 6, 800, 600))
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, prev)
	w, _ := size(img)
	bright := func(x, y int) bool {
		r, _, _, _ := img.At(x, y).RGBA()
		return r>>8 > 128
	}
	if !bright(w-10, 10) {
		t.Error("expected bright block at top-right after orientation 6")
	}
	if bright(10, 10) {
		t.Error("expected dark top-left after orientation 6")
	}
}

func TestGenerateStripsMetadata(t *testing.T) {
	// Embed a JPEG carrying an APP1/Exif segment and make sure it doesn't
	// survive into the outputs.
	var src bytes.Buffer
	mustEncode(t, &src, gradient(800, 600))
	jpg := src.Bytes()
	app1 := append([]byte{0xFF, 0xE1, 0x00, 0x10}, []byte("Exif\x00\x00GPSDATA!")...)
	withExif := append(append(append([]byte{}, jpg[:2]...), app1...), jpg[2:]...)
	raw := append([]byte("II*\x00\x08\x00\x00\x00\x00\x00"), withExif...)

	thumb, prev, err := Generate(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{"thumb": thumb, "preview": prev} {
		if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("GPSDATA")) {
			t.Errorf("%s still contains EXIF data", name)
		}
	}
}

func TestGenerateNoPreview(t *testing.T) {
	raw := append([]byte("II*\x00\x08\x00\x00\x00"), bytes.Repeat([]byte{0xFF, 0xD8, 0xFF, 0x00}, 100)...)
	if _, _, err := Generate(raw); !errors.Is(err, ErrNoPreview) {
		t.Fatalf("err = %v, want ErrNoPreview", err)
	}
}

func TestTiffOrientation(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{"empty", nil, 1},
		{"not tiff", []byte("not a tiff file"), 1},
		{"bad ifd offset", []byte("II*\x00\xff\xff\xff\x7f"), 1},
		{"no orientation tag", []byte("II*\x00\x08\x00\x00\x00\x00\x00"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tiffOrientation(tt.data); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
	for o := 1; o <= 8; o++ {
		if got := tiffOrientation(fakeRaw(t, o, 16, 16)); got != o {
			t.Errorf("orientation %d: got %d", o, got)
		}
	}
}

// TestRealRaws runs Generate over real raw files when PREVIEW_SAMPLE_DIR is
// set, e.g. PREVIEW_SAMPLE_DIR=/abs/path/to/raws go test ./internal/preview -v
func TestRealRaws(t *testing.T) {
	dir := os.Getenv("PREVIEW_SAMPLE_DIR")
	if dir == "" {
		t.Skip("PREVIEW_SAMPLE_DIR not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || (ext != ".arw" && ext != ".nef") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			thumb, prev, err := Generate(raw)
			if err != nil {
				t.Fatal(err)
			}
			tw, th := size(decode(t, thumb))
			pw, ph := size(decode(t, prev))
			t.Logf("thumb %dx%d (%d KB), preview %dx%d (%d KB)", tw, th, len(thumb)/1024, pw, ph, len(prev)/1024)
			if max(tw, th) > ThumbEdge || max(pw, ph) > PreviewEdge {
				t.Error("output exceeds size limits")
			}
		})
	}
}
