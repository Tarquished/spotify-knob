package osd

import (
	"fmt"
	"image"
	"image/color"
	"math"

	"golang.org/x/image/vector"
)

// The panel's outer chrome - the three shadow rings, the border ring, the
// card's own background, the header's accent bloom - only changes shape on
// a resize, and only changes colour when the track's accent, ambient mode or
// the pulse changes. Rebuilding it through the vector rasteriser from
// scratch was, measured, about 85% of a frame's cost: geometry that is
// almost always identical to the frame before it, redone anyway.
//
// This is the same fix the overlay card already applies to its own shadow
// and its bar glow (see cache.go): rasterise once, cache the result, and
// blit it on every later frame instead. It is what turns the pulse from a
// feature that would have made the panel noticeably heavier to keep open
// into one that costs nothing extra once the shape is drawn the first time.
type lyricsCache struct {
	shadowRings [3]maskCache
	border      maskCache
	bloom       maskCache

	bgImg *image.RGBA
	bgKey string
}

// shadowRingMask is the f-th of the three nested rings, f=1..3.
func (w *LyricsWindow) shadowRingMask(f, cx, cy, cw, ch, rad float64) *image.Alpha {
	x, y := cx-f, cy-f
	ww, hh := cw+2*f, ch+2*f
	r := rad + f
	bounds := image.Rect(int(x)-1, int(y)-1, int(x+ww)+2, int(y+hh)+2).
		Intersect(image.Rect(0, 0, w.paint.w, w.paint.h))

	idx := int(f) - 1
	return w.lc.shadowRings[idx].get(newMaskKey(x, y, ww, hh, r), func() *image.Alpha {
		return blurredMask(bounds, 0, func(rr *vector.Rasterizer, ox, oy float32) {
			roundRect(rr, float32(x)-ox, float32(y)-oy, float32(ww), float32(hh), float32(r))
			roundRectRev(rr, float32(x+1)-ox, float32(y+1)-oy, float32(ww-2), float32(hh-2), float32(r-1))
		})
	})
}

// borderRingMask is the hairline ring separating the card from whatever is
// behind it.
func (w *LyricsWindow) borderRingMask(cx, cy, cw, ch, rad float64) *image.Alpha {
	bounds := image.Rect(int(cx)-1, int(cy)-1, int(cx+cw)+2, int(cy+ch)+2).
		Intersect(image.Rect(0, 0, w.paint.w, w.paint.h))
	return w.lc.border.get(newMaskKey(cx, cy, cw, ch, rad), func() *image.Alpha {
		return blurredMask(bounds, 0, func(r *vector.Rasterizer, ox, oy float32) {
			roundRect(r, float32(cx)-ox, float32(cy)-oy, float32(cw), float32(ch), float32(rad))
			roundRectRev(r, float32(cx+1)-ox, float32(cy+1)-oy, float32(cw-2), float32(ch-2), float32(rad-1))
		})
	})
}

// bloomMask is the header's accent glow, cached as its falloff shape
// intersected with the card's own rounded silhouette - geometry only, not
// colour, so the same mask serves every frame regardless of what the pulse's
// brightness is doing that frame.
//
// The falloff alone is not enough: painter.radial used to be clipped to the
// card by being drawn through the same roundRect path the card itself uses,
// and losing that when this became a cached mask let the glow bleed straight
// past the card's rounded top-left corner into the transparent margin around
// it - a visibly square patch where the corner should curve. Clipping the
// mask to the card's rounded rectangle here restores that.
func (w *LyricsWindow) bloomMask(cx, cy, rad, cardX, cardY, cardW, cardH, cardRad float64) *image.Alpha {
	bounds := image.Rect(int(cx-rad), int(cy-rad), int(cx+rad)+1, int(cy+rad)+1).
		Intersect(image.Rect(0, 0, w.paint.w, w.paint.h))
	return w.lc.bloom.get(newMaskKey(cx, cy, rad, cardW, cardH), func() *image.Alpha {
		m := radialFalloffMask(cx, cy, rad, bounds)
		clipToRoundRect(m, cardX, cardY, cardW, cardH, cardRad)
		return m
	})
}

// clipToRoundRect zeroes m's alpha outside a rounded rectangle. A plain
// rectangular bound agrees with a rounded one everywhere except the four
// corners, so only pixels inside a corner's own radius need the distance
// check; everything else is a cheap axis-aligned test.
func clipToRoundRect(m *image.Alpha, x, y, w, h, rad float64) {
	x0, y0, x1, y1 := x, y, x+w, y+h
	b := m.Bounds()
	for py := b.Min.Y; py < b.Max.Y; py++ {
		fy := float64(py) + 0.5
		for px := b.Min.X; px < b.Max.X; px++ {
			fx := float64(px) + 0.5
			if !insideRoundRect(fx, fy, x0, y0, x1, y1, rad) {
				m.SetAlpha(px, py, color.Alpha{})
			}
		}
	}
}

func insideRoundRect(px, py, x0, y0, x1, y1, rad float64) bool {
	if px < x0 || px > x1 || py < y0 || py > y1 {
		return false
	}
	cx, cy := px, py
	switch {
	case px < x0+rad && py < y0+rad:
		cx, cy = x0+rad, y0+rad
	case px > x1-rad && py < y0+rad:
		cx, cy = x1-rad, y0+rad
	case px < x0+rad && py > y1-rad:
		cx, cy = x0+rad, y1-rad
	case px > x1-rad && py > y1-rad:
		cx, cy = x1-rad, y1-rad
	default:
		return true // not within a corner's own radius: the plain box already agrees
	}
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= rad*rad
}

// radialFalloffMask bakes painter.radial's own squared falloff into an alpha
// mask once, so a pulsing glow is a cached lookup and a blit rather than the
// same per-pixel distance-and-falloff math redone on every one of its frames.
func radialFalloffMask(cx, cy, rad float64, bounds image.Rectangle) *image.Alpha {
	m := image.NewAlpha(bounds)
	if rad <= 0 {
		return m
	}
	inv := 1 / rad
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		dy := float64(y) - cy
		dy2 := dy * dy
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dx := float64(x) - cx
			d := math.Sqrt(dx*dx+dy2) * inv
			a := 0.0
			if d < 1 {
				f := 1 - d
				a = f * f
			}
			m.SetAlpha(x, y, color.Alpha{A: uint8(a*255 + 0.5)})
		}
	}
	return m
}

// cardBackground is the card's fill: the flat gradient normally, or the
// blurred cover with its scrim already baked in when ambient mode is on.
// Cached on whichever inputs can change it, so a resize, a new cover or
// toggling ambient mode rebuilds it once rather than every frame.
func (w *LyricsWindow) cardBackground(cw, ch int) *image.RGBA {
	if w.ambientOn && w.lastArt != nil && w.lastArt.img != nil {
		key := fmt.Sprintf("amb|%s|%d|%d", w.artURL, cw, ch)
		if key == w.lc.bgKey {
			return w.lc.bgImg
		}
		img := buildAmbientBackground(w.lastArt.img, cw, ch)
		// A dark scrim over the photo, tuned so text and every control read
		// exactly as legibly as they do over the flat card - this is a
		// backdrop treatment, not a lyrics-on-a-photo redesign. Baked in
		// once here rather than drawn as a separate flat() every frame.
		applyScrim(img, rgba(8, 8, 10, 0.66))
		w.lc.bgImg, w.lc.bgKey = img, key
		return img
	}

	key := fmt.Sprintf("flat|%d|%d", cw, ch)
	if key == w.lc.bgKey {
		return w.lc.bgImg
	}
	img := buildCardGradient(cw, ch)
	w.lc.bgImg, w.lc.bgKey = img, key
	return img
}

// buildCardGradient is the same top-to-bottom graphite fill the card always
// used, precomputed as a plain rectangular tile; the rounded-corner clip is
// still applied fresh each frame when it is blitted in, which costs nothing
// next to the per-pixel gradient math this replaces.
func buildCardGradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 {
		return img
	}
	fr, fg, fb, fa := float64(colCardTop.R), float64(colCardTop.G), float64(colCardTop.B), float64(colCardTop.A)
	dr := float64(colCardBottom.R) - fr
	dg := float64(colCardBottom.G) - fg
	db := float64(colCardBottom.B) - fb
	da := float64(colCardBottom.A) - fa

	for y := 0; y < h; y++ {
		t := 0.0
		if h > 1 {
			t = float64(y) / float64(h-1)
		}
		r := uint8(fr + dr*t)
		g := uint8(fg + dg*t)
		b := uint8(fb + db*t)
		a := uint8(fa + da*t)
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			row[x*4+0], row[x*4+1], row[x*4+2], row[x*4+3] = r, g, b, a
		}
	}
	return img
}

// applyScrim darkens img in place with a flat premultiplied colour, source
// over. Used once when an ambient background is built rather than as a
// separate full-card flat() every frame.
func applyScrim(img *image.RGBA, col color.RGBA) {
	if img == nil {
		return
	}
	sr, sg, sb, sa := uint32(col.R), uint32(col.G), uint32(col.B), uint32(col.A)
	ia := 255 - sa
	for i := 0; i+3 < len(img.Pix); i += 4 {
		img.Pix[i+0] = uint8(sr + uint32(img.Pix[i+0])*ia/255)
		img.Pix[i+1] = uint8(sg + uint32(img.Pix[i+1])*ia/255)
		img.Pix[i+2] = uint8(sb + uint32(img.Pix[i+2])*ia/255)
		img.Pix[i+3] = uint8(sa + uint32(img.Pix[i+3])*ia/255)
	}
}
