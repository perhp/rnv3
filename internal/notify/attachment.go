package notify

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

// attachmentBytes returns the file's bytes, shrinking a JPEG by 20% steps
// until it fits maxBytes (RN2 resized to 80% when over Pushover's limit).
func attachmentBytes(path string, maxBytes int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) <= maxBytes {
		return data, nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".jpg" && ext != ".jpeg" {
		return nil, fmt.Errorf("%s is %d bytes, over the %d limit, and only JPEGs are resized", filepath.Base(path), len(data), maxBytes)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for i := 0; i < 8; i++ {
		b := img.Bounds()
		w, h := b.Dx()*4/5, b.Dy()*4/5
		if w < 16 || h < 16 {
			break
		}
		small := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(small, small.Bounds(), img, b, draw.Over, nil)
		img = small
		var out bytes.Buffer
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
		if out.Len() <= maxBytes {
			return out.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("%s cannot be shrunk under %d bytes", filepath.Base(path), maxBytes)
}
