// Package process is the image pipeline between SatDump's raw work-dir
// products and finished gallery imagery: rename/flip/dedup rules, JPEG
// normalization, thumbnails, polar plots, the sky map, and daily artifacts.
// Replaces RN2's ImageMagick/matplotlib post-processing.
package process

import (
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
)

// thumbnailWidth matches RN2's thumbnail.sh invocations.
const thumbnailWidth = 300

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

func saveJPEG(path string, img image.Image, quality int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// rotate180 flips an image upside down (RN2's `convert -rotate 180` for
// Northbound passes).
func rotate180(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			out.Set(b.Max.X-1-x-b.Min.X, b.Max.Y-1-y-b.Min.Y, img.At(x, y))
		}
	}
	return out
}

// resizeToWidth scales an image to the given width, preserving aspect ratio.
func resizeToWidth(img image.Image, width int) image.Image {
	b := img.Bounds()
	if b.Dx() <= width || b.Dx() == 0 {
		return img
	}
	height := int(float64(b.Dy()) * float64(width) / float64(b.Dx()))
	if height < 1 {
		height = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, b, xdraw.Over, nil)
	return out
}

// normalize writes the final JPEG and its thumbnail for one product image.
func normalize(img image.Image, flip bool, imagePath, thumbPath string, quality int) error {
	if flip {
		img = rotate180(img)
	}
	if err := saveJPEG(imagePath, img, quality); err != nil {
		return err
	}
	return saveJPEG(thumbPath, resizeToWidth(img, thumbnailWidth), quality)
}

// lightenBlend merges src onto dst keeping the brighter pixel per channel —
// RN2's `convert -compose lighten -flatten` for daily mosaics. Images must
// share dimensions (SatDump's projection grids are fixed and
// station-anchored); a mismatched frame is the caller's problem.
func lightenBlend(dst *image.RGBA, src image.Image) {
	b := dst.Bounds()
	sb := src.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			sr, sg, sbl, _ := src.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
			i := dst.PixOffset(b.Min.X+x, b.Min.Y+y)
			if v := uint8(sr >> 8); v > dst.Pix[i] {
				dst.Pix[i] = v
			}
			if v := uint8(sg >> 8); v > dst.Pix[i+1] {
				dst.Pix[i+1] = v
			}
			if v := uint8(sbl >> 8); v > dst.Pix[i+2] {
				dst.Pix[i+2] = v
			}
			dst.Pix[i+3] = 255
		}
	}
}

// toRGBA copies any image into an RGBA canvas.
func toRGBA(img image.Image) *image.RGBA {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out
}
