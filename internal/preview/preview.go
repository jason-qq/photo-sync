// Package preview turns a TIFF-based raw file (Sony ARW, Nikon NEF) into the
// two JPEG renditions the review app serves: a grid thumbnail and a larger
// judging preview.
//
// It works entirely in pure Go: the largest JPEG embedded in the raw is
// decoded (no raw sensor decode), rotated upright using the raw's TIFF
// Orientation tag, resized, and re-encoded. Re-encoding writes no metadata,
// so EXIF (including GPS) is stripped as a side effect.
package preview

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"sort"

	"github.com/disintegration/imaging"
)

const (
	// ThumbEdge is the maximum long edge, in pixels, of the grid thumbnail.
	ThumbEdge = 400
	// PreviewEdge is the maximum long edge, in pixels, of the judging preview.
	// Images smaller than this are not upscaled.
	PreviewEdge = 2048
	// Quality is the JPEG quality used for both renditions.
	Quality = 85
)

// ErrNoPreview is returned when the raw contains no decodable embedded JPEG.
var ErrNoPreview = errors.New("preview: no decodable embedded JPEG found")

// Generate returns JPEG-encoded thumb and preview renditions of raw.
func Generate(raw []byte) (thumb, preview []byte, err error) {
	img, err := largestEmbeddedJPEG(raw)
	if err != nil {
		return nil, nil, err
	}
	img = applyOrientation(img, tiffOrientation(raw))

	thumb, err = encode(fit(img, ThumbEdge))
	if err != nil {
		return nil, nil, err
	}
	preview, err = encode(fit(img, PreviewEdge))
	if err != nil {
		return nil, nil, err
	}
	return thumb, preview, nil
}

// fit scales img down so its long edge is at most edge, preserving aspect
// ratio. Images already within bounds are returned unchanged.
func fit(img image.Image, edge int) image.Image {
	if b := img.Bounds(); b.Dx() <= edge && b.Dy() <= edge {
		return img
	}
	return imaging.Fit(img, edge, edge, imaging.Lanczos)
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: Quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// largestEmbeddedJPEG scans for JPEG start-of-image markers and returns the
// largest image that fully decodes. Lossless-JPEG raw sensor data (SOF3) and
// false-positive markers are rejected by image/jpeg and skipped.
func largestEmbeddedJPEG(data []byte) (image.Image, error) {
	type candidate struct{ off, area int }
	imageStart := []byte{0xFF, 0xD8, 0xFF}

	var cands []candidate
	for i := 0; i < len(data); {
		j := bytes.Index(data[i:], imageStart)
		if j < 0 {
			break
		}
		off := i + j
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[off:])); err == nil {
			cands = append(cands, candidate{off, cfg.Width * cfg.Height})
		}
		i = off + len(imageStart)
	}

	sort.SliceStable(cands, func(a, b int) bool { return cands[a].area > cands[b].area })
	for _, c := range cands {
		if img, err := jpeg.Decode(bytes.NewReader(data[c.off:])); err == nil {
			return img, nil
		}
	}
	return nil, ErrNoPreview
}

// tiffOrientation reads the Orientation tag (0x0112) from IFD0 of a
// TIFF-based raw. It returns 1 (normal) if the data isn't TIFF, the tag is
// absent, or the value is out of range.
func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(data[2:4]) != 42 {
		return 1
	}
	ifd := int(bo.Uint32(data[4:8]))
	if ifd < 8 || ifd+2 > len(data) {
		return 1
	}
	n := int(bo.Uint16(data[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + k*12
		if e+12 > len(data) {
			break
		}
		if bo.Uint16(data[e:]) == 0x0112 {
			if v := int(bo.Uint16(data[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// applyOrientation transforms img so it displays upright for the given EXIF
// orientation value.
func applyOrientation(img image.Image, o int) image.Image {
	switch o {
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.Transpose(img)
	case 6:
		return imaging.Rotate270(img) // 90° clockwise
	case 7:
		return imaging.Transverse(img)
	case 8:
		return imaging.Rotate90(img) // 90° counter-clockwise
	}
	return img
}
