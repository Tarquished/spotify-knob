package osd

import (
	"image"
	"math"

	"golang.org/x/image/vector"
)

// Blurring is by far the most expensive thing in a frame, and the shapes
// being blurred barely move: the shadow is fixed for the life of a card, and
// the bar's bloom only changes while the volume is gliding. Caching the
// blurred masks turns a per-frame cost into a per-change one.

type maskKey struct{ a, b, c, d, e int32 }

// newMaskKey quantises to half-pixel steps: finer than the eye can resolve
// through a blur, coarse enough that sub-pixel jitter does not defeat the
// cache.
func newMaskKey(v ...float64) maskKey {
	var k [5]int32
	for i := 0; i < len(v) && i < 5; i++ {
		k[i] = int32(math.Round(v[i] * 2))
	}
	return maskKey{k[0], k[1], k[2], k[3], k[4]}
}

type maskCache struct {
	key   maskKey
	value *image.Alpha
	valid bool
}

func (m *maskCache) get(key maskKey, build func() *image.Alpha) *image.Alpha {
	if m.valid && m.key == key {
		return m.value
	}
	m.key = key
	m.value = build()
	m.valid = true
	return m.value
}

func (o *OSD) shadowMask(x, y, w, h, rad float64) *image.Alpha {
	l := o.layout
	return o.shadow.get(newMaskKey(x, y, w, h, rad), func() *image.Alpha {
		spread := l.shadowBlur*3 + 2
		bounds := image.Rect(
			int(x)-spread, int(y)-spread,
			int(x+w)+spread, int(y+h)+spread,
		).Intersect(image.Rect(0, 0, int(l.canvasW), int(l.canvasH)))

		return blurredMask(bounds, l.shadowBlur, func(r *vector.Rasterizer, ox, oy float32) {
			roundRect(r, float32(x)-ox, float32(y)-oy, float32(w), float32(h), float32(rad))
		})
	})
}

func (o *OSD) barGlowMask(x, y, w, h, rad float64) *image.Alpha {
	l := o.layout
	radius := int(l.px(5))
	return o.barGlow.get(newMaskKey(x, y, w, h, rad), func() *image.Alpha {
		spread := radius*3 + 2
		bounds := image.Rect(
			int(x)-spread, int(y)-spread,
			int(x+w)+spread, int(y+h)+spread,
		).Intersect(image.Rect(0, 0, int(l.canvasW), int(l.canvasH)))

		return blurredMask(bounds, radius, func(r *vector.Rasterizer, ox, oy float32) {
			roundRect(r, float32(x)-ox, float32(y)-oy, float32(w), float32(h), float32(rad))
		})
	})
}
