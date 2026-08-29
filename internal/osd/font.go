package osd

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

type weight int

const (
	regular weight = iota
	semibold
	bold
)

// fontSet resolves Segoe UI from the Windows font directory and caches a face
// per (weight, pixel size). The Go font is embedded as a fallback so text
// always renders, even on a stripped-down system.
type fontSet struct {
	mu    sync.Mutex
	fonts map[weight]*sfnt.Font
	faces map[faceKey]font.Face
}

type faceKey struct {
	w    weight
	size int // pixel size * 16, so fractional sizes still key cleanly
}

func newFontSet() *fontSet {
	fs := &fontSet{
		fonts: make(map[weight]*sfnt.Font),
		faces: make(map[faceKey]font.Face),
	}
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	candidates := map[weight][]string{
		regular:  {"segoeui.ttf"},
		semibold: {"seguisb.ttf", "segoeuib.ttf", "segoeui.ttf"},
		bold:     {"segoeuib.ttf", "seguisb.ttf", "segoeui.ttf"},
	}
	for w, names := range candidates {
		for _, n := range names {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				continue
			}
			f, err := opentype.Parse(b)
			if err != nil {
				continue
			}
			fs.fonts[w] = f
			break
		}
	}
	if len(fs.fonts) == 0 {
		if f, err := opentype.Parse(goregular.TTF); err == nil {
			fs.fonts[regular] = f
			fs.fonts[semibold] = f
			fs.fonts[bold] = f
		}
	}
	return fs
}

// face returns a cached face at the given pixel size.
func (fs *fontSet) face(w weight, px float64) font.Face {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	key := faceKey{w: w, size: int(px * 16)}
	if f, ok := fs.faces[key]; ok {
		return f
	}
	src, ok := fs.fonts[w]
	if !ok {
		src = fs.fonts[regular]
	}
	if src == nil {
		return nil
	}
	// DPI 72 makes Size map 1:1 onto pixels; the caller has already applied
	// the display scale.
	f, err := opentype.NewFace(src, &opentype.FaceOptions{
		Size:    px,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil
	}
	fs.faces[key] = f
	return f
}

func measure(f font.Face, s string) float64 {
	if f == nil {
		return 0
	}
	return float64(font.MeasureString(f, s)) / 64
}

// truncate shortens s with an ellipsis so it fits maxW pixels.
func truncate(f font.Face, s string, maxW float64) string {
	if f == nil || measure(f, s) <= maxW {
		return s
	}
	const ell = "…"
	runes := []rune(s)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + ell
		if measure(f, candidate) <= maxW {
			// Do not leave a dangling space before the ellipsis.
			for len(runes) > 0 && runes[len(runes)-1] == ' ' {
				runes = runes[:len(runes)-1]
				candidate = string(runes) + ell
			}
			return candidate
		}
	}
	return ell
}

// drawText renders s with its baseline at y.
func drawText(dst *image.RGBA, f font.Face, col color.Color, x, y float64, s string) {
	if f == nil || s == "" {
		return
	}
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: f,
		Dot:  fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(y * 64)},
	}
	d.DrawString(s)
}

// drawTracked renders s with extra letter spacing, for the small uppercase
// labels where tight default spacing reads as cramped.
func drawTracked(dst *image.RGBA, f font.Face, col color.Color, x, y float64, s string, tracking float64) {
	if f == nil {
		return
	}
	for _, r := range s {
		g := string(r)
		drawText(dst, f, col, x, y, g)
		x += measure(f, g) + tracking
	}
}

func measureTracked(f font.Face, s string, tracking float64) float64 {
	if f == nil || s == "" {
		return 0
	}
	w := 0.0
	n := 0
	for _, r := range s {
		w += measure(f, string(r)) + tracking
		n++
	}
	if n > 0 {
		w -= tracking
	}
	return w
}
