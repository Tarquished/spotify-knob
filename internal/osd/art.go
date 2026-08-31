package osd

import (
	"context"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
)

// artwork is a decoded, scaled album cover plus the accent colour sampled
// from it. The accent is what ties the card to whatever is playing: the
// volume bar, the halo and the direction chip all take their colour from the
// cover art rather than from a fixed palette.
type artwork struct {
	img    *image.RGBA // cover at card size
	thumb  *image.RGBA // cover at queue-row size
	accent color.RGBA

	// accents is a handful of accent colours swept left to right across the
	// cover, for the lyrics panel's slow colour journey (see liveAccent in
	// ambient.go). accent above is the single, unchanging colour every other
	// surface still uses; this is additional, not a replacement for it.
	accents []color.RGBA
}

// artCache fetches covers once and keeps a small number around. Album art
// URLs repeat constantly (same track, volume nudged ten times), so even a
// tiny cache removes almost all network work.
type artCache struct {
	mu      sync.Mutex
	entries map[string]*artwork
	order   []string
	inWork  map[string]bool
	hc      *http.Client
	size    int
	thumb   int
}

const artCacheMax = 12

func newArtCache(size, thumb int) *artCache {
	return &artCache{
		entries: make(map[string]*artwork),
		inWork:  make(map[string]bool),
		hc:      &http.Client{Timeout: 6 * time.Second},
		size:    size,
		thumb:   thumb,
	}
}

// get returns cached artwork, or nil if it is not loaded yet.
func (c *artCache) get(url string) *artwork {
	if url == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[url]
}

// fetch loads the artwork in the background and calls done when it lands.
// Concurrent requests for the same URL collapse into one download.
func (c *artCache) fetch(ctx context.Context, url string, done func(*artwork)) {
	if url == "" {
		return
	}
	c.mu.Lock()
	if a, ok := c.entries[url]; ok {
		c.mu.Unlock()
		done(a)
		return
	}
	if c.inWork[url] {
		c.mu.Unlock()
		return
	}
	c.inWork[url] = true
	c.mu.Unlock()

	go func() {
		a := c.load(ctx, url)
		c.mu.Lock()
		delete(c.inWork, url)
		if a != nil {
			c.entries[url] = a
			c.order = append(c.order, url)
			for len(c.order) > artCacheMax {
				delete(c.entries, c.order[0])
				c.order = c.order[1:]
			}
		}
		c.mu.Unlock()
		if a != nil {
			done(a)
		}
	}()
}

func (c *artCache) load(ctx context.Context, url string) *artwork {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	src, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil
	}

	// One decode, both sizes: the queue rows need a small thumbnail and the
	// card needs the large cover, and downloading twice would be silly.
	dst := image.NewRGBA(image.Rect(0, 0, c.size, c.size))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)

	th := image.NewRGBA(image.Rect(0, 0, c.thumb, c.thumb))
	xdraw.CatmullRom.Scale(th, th.Bounds(), src, src.Bounds(), xdraw.Src, nil)

	return &artwork{img: dst, thumb: th, accent: accentFrom(src), accents: accentJourney(src)}
}

// accentJourney runs accentFrom separately over a handful of vertical
// strips swept left to right across src, instead of over the whole image at
// once. A single accent already summarises a cover; this keeps the pieces
// that summary was built from, so a colour can drift across them instead of
// sitting on one average forever - see liveAccent in ambient.go.
//
// Built from the same decoded image the single accent already reads, and
// only once per cover load, not per frame.
func accentJourney(src image.Image) []color.RGBA {
	const stops = 6
	b := src.Bounds()
	w := b.Dx()
	if w <= 0 {
		return nil
	}
	sub, ok := src.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		// Every decoder in this binary's image registry (jpeg, png) returns
		// a type that implements this; if some future one somehow does not,
		// falling back to one repeated colour is a plain accent, not a
		// crash.
		one := accentFrom(src)
		out := make([]color.RGBA, stops)
		for i := range out {
			out[i] = one
		}
		return out
	}

	out := make([]color.RGBA, stops)
	for i := 0; i < stops; i++ {
		x0 := b.Min.X + i*w/stops
		x1 := b.Min.X + (i+1)*w/stops
		if x1 <= x0 {
			x1 = x0 + 1
		}
		strip := image.Rect(x0, b.Min.Y, x1, b.Max.Y)
		out[i] = accentFrom(sub.SubImage(strip))
	}
	return out
}

// accentFrom picks a colour that represents the cover without being muddy.
//
// Averaging every pixel gives grey, and picking the single most common colour
// usually lands on a near-black background. Instead each pixel votes with a
// weight based on how colourful and how mid-toned it is, hues are averaged on
// the colour wheel so red does not cancel with magenta, and the result is
// pushed into a band that stays readable on a dark card.
func accentFrom(src image.Image) color.RGBA {
	const grid = 24
	small := image.NewRGBA(image.Rect(0, 0, grid, grid))
	xdraw.ApproxBiLinear.Scale(small, small.Bounds(), src, src.Bounds(), xdraw.Src, nil)

	var sinSum, cosSum, satSum, litSum, weight float64
	for y := 0; y < grid; y++ {
		for x := 0; x < grid; x++ {
			r, g, b, _ := small.At(x, y).RGBA()
			h, s, l := rgbToHSL(float64(r)/65535, float64(g)/65535, float64(b)/65535)
			if s < 0.12 || l < 0.10 || l > 0.94 {
				continue // grey, near-black or blown-out: no opinion
			}
			// Favour saturated, mid-toned pixels.
			w := s * (1 - math.Abs(l-0.5)*1.3)
			if w <= 0 {
				continue
			}
			rad := h * 2 * math.Pi
			sinSum += math.Sin(rad) * w
			cosSum += math.Cos(rad) * w
			satSum += s * w
			litSum += l * w
			weight += w
		}
	}
	if weight == 0 {
		return colAccentFallback
	}

	h := math.Atan2(sinSum, cosSum) / (2 * math.Pi)
	if h < 0 {
		h++
	}
	s := satSum / weight
	l := litSum / weight

	// Keep it vivid and legible against the dark card.
	s = math.Max(s, 0.62)
	s = math.Min(s, 0.95)
	l = math.Max(l, 0.52)
	l = math.Min(l, 0.68)

	r, g, b := hslToRGB(h, s, l)
	return color.RGBA{R: uint8(r * 255), G: uint8(g * 255), B: uint8(b * 255), A: 255}
}

func rgbToHSL(r, g, b float64) (h, s, l float64) {
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	if max == min {
		return 0, 0, l
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

func hslToRGB(h, s, l float64) (r, g, b float64) {
	if s == 0 {
		return l, l, l
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	return hueToRGB(p, q, h+1.0/3), hueToRGB(p, q, h), hueToRGB(p, q, h-1.0/3)
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6:
		return p + (q-p)*6*t
	case t < 1.0/2:
		return q
	case t < 2.0/3:
		return p + (q-p)*(2.0/3-t)*6
	default:
		return p
	}
}
