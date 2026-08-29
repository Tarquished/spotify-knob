package osd

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// outDir lets a human point the render dump somewhere they can look at it:
//
//	OSD_OUT=C:\tmp go test ./internal/osd -run TestRenderSamples
func outDir(t *testing.T) string {
	if d := os.Getenv("OSD_OUT"); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	return t.TempDir()
}

// fakeCover builds a plausible album cover so the artwork path, the fade and
// the accent extraction all get exercised without hitting the network.
func fakeCover(size int, base color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := float64(x) / float64(size)
			fy := float64(y) / float64(size)
			d := math.Hypot(fx-0.35, fy-0.3)
			f := math.Max(0, 1-d*1.3)
			img.Set(x, y, color.RGBA{
				R: uint8(float64(base.R)*f + 12),
				G: uint8(float64(base.G)*f + 10),
				B: uint8(float64(base.B)*f + 18),
				A: 255,
			})
		}
	}
	// A darker band, the way a lot of covers have a photo plus a solid block.
	for y := size * 3 / 4; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{R: 18, G: 16, B: 22, A: 255})
		}
	}
	return img
}

func testArtwork(t *testing.T, o *OSD, base color.RGBA) *artwork {
	t.Helper()
	src := fakeCover(300, base)
	size := int(o.layout.artSize)
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	scaleInto(dst, src)
	return &artwork{img: dst, accent: accentFrom(src)}
}

func scaleInto(dst *image.RGBA, src image.Image) {
	b := dst.Bounds()
	sb := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sx := sb.Min.X + x*sb.Dx()/b.Dx()
			sy := sb.Min.Y + y*sb.Dy()/b.Dy()
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}

func save(t *testing.T, dir, name string, img *image.RGBA) {
	t.Helper()
	// Composite onto a mid-grey checkerboard so the transparency and the
	// shadow are visible in the dump instead of blending into white.
	out := image.NewRGBA(img.Bounds())
	for y := out.Bounds().Min.Y; y < out.Bounds().Max.Y; y++ {
		for x := out.Bounds().Min.X; x < out.Bounds().Max.X; x++ {
			v := uint8(96)
			if (x/12+y/12)%2 == 0 {
				v = 122
			}
			out.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			i := img.PixOffset(x, y)
			sr, sg, sb, sa := img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]
			j := out.PixOffset(x, y)
			ia := 255 - uint32(sa)
			out.Pix[j+0] = uint8(uint32(sr) + uint32(out.Pix[j+0])*ia/255)
			out.Pix[j+1] = uint8(uint32(sg) + uint32(out.Pix[j+1])*ia/255)
			out.Pix[j+2] = uint8(uint32(sb) + uint32(out.Pix[j+2])*ia/255)
		}
	}

	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, out); err != nil {
		t.Fatal(err)
	}
}

// TestRenderSamples renders every card variant. It asserts the renderer does
// not panic and produces non-empty pixels; the PNGs it leaves behind are for
// eyeballing the design.
func TestRenderSamples(t *testing.T) {
	dir := outDir(t)
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	art := testArtwork(t, o, color.RGBA{R: 226, G: 88, B: 54, A: 255})

	cases := []struct {
		name string
		st   frameState
	}{
		{"volume-mid", frameState{
			kind: KindVolume, volume: 45, bar: 0.45, progress: 0.36,
			elapsed: 114 * time.Second, total: 318 * time.Second,
			title: "Weird Fishes / Arpeggi", artist: "Radiohead",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"volume-full", frameState{
			kind: KindVolume, volume: 100, bar: 1, progress: 0.97,
			elapsed: 305 * time.Second, total: 314 * time.Second,
			title: "Nights", artist: "Frank Ocean",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"volume-zero", frameState{
			kind: KindVolume, volume: 0, bar: 0, progress: 0.004,
			elapsed: 1 * time.Second, total: 265 * time.Second,
			title:  "A Very Long Track Title That Has To Be Truncated Somewhere",
			artist: "Some Artist, Another Artist, A Third One",
			art:    art, accent: art.accent, artFade: 1,
		}},
		{"volume-no-art", frameState{
			kind: KindVolume, volume: 65, bar: 0.65,
			title: "Midnight City", artist: "M83",
			accent: colAccentFallback,
		}},
		{"track-next", frameState{
			kind: KindTrack, dir: Forward, progress: 0.5,
			elapsed: 119 * time.Second, total: 238 * time.Second,
			title: "Sunset Lover", artist: "Petit Biscuit",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"track-previous", frameState{
			kind: KindTrack, dir: Backward, progress: 0.74,
			elapsed: 4*time.Minute + 3*time.Second, total: 5*time.Minute + 27*time.Second,
			title: "Redbone", artist: "Childish Gambino",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"track-pending", frameState{
			kind: KindTrack, dir: Forward, pending: true,
			accent: colAccentFallback,
		}},
		{"peek", frameState{
			kind: KindPeek, accent: art.accent, selected: 1,
			queue: []Track{
				{Title: "Sesame Syrup", Artist: "Cigarettes After Sex"},
				{Title: "What You Heard", Artist: "Sonder"},
				{Title: "Everyone Adores You (at least I do)", Artist: "Matt Maltese"},
				{Title: "heaven and hell", Artist: "wave to earth"},
			},
			queueArt: []*artwork{art, art, nil, art},
		}},
		{"peek-first-row", frameState{
			kind: KindPeek, accent: colAccentFallback, selected: 0,
			queue: []Track{
				{Title: "Die For You", Artist: "Joji"},
				{Title: "Beaches", Artist: "beabadoobee"},
				{Title: "Second Best", Artist: "Laufey"},
				{Title: "Redbone", Artist: "Childish Gambino"},
			},
			queueArt: []*artwork{nil, nil, nil, nil},
		}},
		{"volume-long-track", frameState{
			kind: KindVolume, volume: 45, bar: 0.45, progress: 0.38,
			elapsed: 41*time.Minute + 9*time.Second, total: time.Hour + 48*time.Minute,
			title: "Weird Fishes / Arpeggi", artist: "Radiohead",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"track-no-duration", frameState{
			kind: KindTrack, dir: Forward, progress: 0.12,
			title: "Sunset Lover", artist: "Petit Biscuit",
			art: art, accent: art.accent, artFade: 1,
		}},
		{"notice-no-lyrics", frameState{
			kind: KindNotice, label: "LYRICS", progress: 0.42,
			elapsed: 88 * time.Second, total: 210 * time.Second,
			title:  "No lyrics for this track",
			artist: "Weird Fishes / Arpeggi",
			art:    art, accent: art.accent, artFade: 1,
		}},
		{"art-fading-in", frameState{
			kind: KindVolume, volume: 45, bar: 0.45,
			title: "Weird Fishes / Arpeggi", artist: "Radiohead",
			art: art, accent: art.accent, artFade: 0.4,
		}},
	}

	for _, tc := range cases {
		o.renderFrame(tc.st)
		if !hasPixels(o.paint.dst) {
			t.Fatalf("%s rendered an empty frame", tc.name)
		}
		save(t, dir, tc.name+".png", o.paint.dst)
	}
	t.Log("frames written to", dir)
}

// Accent extraction must reject the near-black background and settle on the
// colourful part of the cover.
func TestAccentIgnoresDarkBackground(t *testing.T) {
	got := accentFrom(fakeCover(200, color.RGBA{R: 226, G: 88, B: 54, A: 255}))
	_, _, l := rgbToHSL(float64(got.R)/255, float64(got.G)/255, float64(got.B)/255)
	if l < 0.45 || l > 0.72 {
		t.Fatalf("accent lightness %.2f is outside the readable band (%v)", l, got)
	}
	if got == colAccentFallback {
		t.Fatal("fell back to the default accent on a clearly colourful cover")
	}
}

// A cover with no colour at all should fall back rather than produce mud.
func TestAccentFallsBackOnGreyscale(t *testing.T) {
	grey := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8(40 + x)
			grey.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	if got := accentFrom(grey); got != colAccentFallback {
		t.Fatalf("want fallback accent for a greyscale cover, got %v", got)
	}
}

func TestTruncateAddsEllipsis(t *testing.T) {
	fs := newFontSet()
	f := fs.face(regular, 14)
	if f == nil {
		t.Skip("no font available")
	}
	long := "An extremely long track title that will not fit in the card"
	got := truncate(f, long, 120)
	if got == long {
		t.Fatal("expected truncation")
	}
	if measure(f, got) > 120 {
		t.Fatalf("truncated string still too wide: %.1f", measure(f, got))
	}
}

func hasPixels(img *image.RGBA) bool {
	for _, v := range img.Pix {
		if v != 0 {
			return true
		}
	}
	return false
}
