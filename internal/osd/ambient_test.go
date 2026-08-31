package osd

import (
	"image"
	"image/color"
	"testing"
	"time"
)

// Clicking the cover flips ambient mode; clicking anywhere else in the
// header does not.
func TestClickingCoverTogglesAmbientMode(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540, visible: true}
	w.track = LyricsTrack{Title: "T", Artist: "A"}

	ax, ay, asize := w.coverRect()
	cx, cy := int(ax+asize/2), int(ay+asize/2)

	if w.ambientOn {
		t.Fatal("ambient mode should start off")
	}
	w.onMouseDown(cx, cy)
	if !w.ambientOn {
		t.Fatal("clicking the cover should turn ambient mode on")
	}
	w.onMouseDown(cx, cy)
	if w.ambientOn {
		t.Fatal("clicking the cover again should turn it back off")
	}
}

func TestHitZoneMatchesTheCover(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	ax, ay, asize := w.coverRect()

	if got := w.hitZone(int(ax+asize/2), int(ay+asize/2)); got != zoneAmbient {
		t.Fatalf("the cover's own centre should hit zoneAmbient, got %v", got)
	}
	// Well outside the cover, still in the header band, should fall back to
	// the ordinary drag-to-move zone rather than also toggling ambient mode.
	if got := w.hitZone(int(ax+asize+40), int(ay)); got == zoneAmbient {
		t.Fatal("well past the cover should not hit it")
	}
}

// buildAmbientBackground must produce an image sized to what was asked for,
// and must not panic on a degenerate source or target size.
func TestBuildAmbientBackgroundSizesCorrectly(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 46, 46))
	for y := 0; y < 46; y++ {
		for x := 0; x < 46; x++ {
			src.Set(x, y, image.NewUniform(image.Black).At(0, 0))
		}
	}
	got := buildAmbientBackground(src, 300, 200)
	if got == nil {
		t.Fatal("want an image back")
	}
	if b := got.Bounds(); b.Dx() != 300 || b.Dy() != 200 {
		t.Fatalf("want 300x200, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestBuildAmbientBackgroundRefusesNonsense(t *testing.T) {
	if got := buildAmbientBackground(nil, 100, 100); got != nil {
		t.Fatalf("nil source should give nil, got %v", got)
	}
	src := image.NewRGBA(image.Rect(0, 0, 46, 46))
	if got := buildAmbientBackground(src, 0, 0); got != nil {
		t.Fatalf("a zero target size should give nil, got %v", got)
	}
}

// accentAlong must return exactly the stop at each end, and something
// between two neighbours strictly in between - the whole point being a
// continuous drift rather than a handful of visible jump cuts.
func TestAccentAlongInterpolatesBetweenNeighbouringStops(t *testing.T) {
	stops := []color.RGBA{
		{R: 0, G: 0, B: 0, A: 255},
		{R: 100, G: 0, B: 0, A: 255},
		{R: 200, G: 0, B: 0, A: 255},
	}
	if got := accentAlong(stops, 0); got != stops[0] {
		t.Fatalf("want the first stop at f=0, got %v", got)
	}
	if got := accentAlong(stops, 1); got != stops[2] {
		t.Fatalf("want the last stop at f=1, got %v", got)
	}
	mid := accentAlong(stops, 0.25) // a quarter of the way: halfway across the first segment
	if mid.R <= stops[0].R || mid.R >= stops[1].R {
		t.Fatalf("want something strictly between stop 0 and stop 1, got %v", mid)
	}
}

func TestAccentAlongHandlesDegenerateInput(t *testing.T) {
	if got := accentAlong(nil, 0.5); got != colAccentFallback {
		t.Fatalf("no stops at all should fall back, got %v", got)
	}
	one := []color.RGBA{{R: 9, G: 9, B: 9, A: 255}}
	if got := accentAlong(one, 0.5); got != one[0] {
		t.Fatalf("a single stop should just be itself regardless of f, got %v", got)
	}
}

// liveAccent must stay put - the same static colour every other surface
// already used - unless the cover yielded a real journey and the track's
// length is actually known to place a position along it.
func TestLiveAccentFallsBackToTheStaticAccentWithoutAJourney(t *testing.T) {
	w := newTestPanel()
	w.accent = color.RGBA{R: 40, G: 60, B: 80, A: 255}
	w.track = LyricsTrack{Duration: time.Minute, Position: 30 * time.Second, PositionAt: time.Now()}

	if got := w.liveAccent(time.Now()); got != w.accent {
		t.Fatalf("no artwork at all: want the static accent, got %v", got)
	}

	w.lastArt = &artwork{accent: w.accent, accents: []color.RGBA{w.accent}}
	if got := w.liveAccent(time.Now()); got != w.accent {
		t.Fatalf("only one journey stop: want the static accent, got %v", got)
	}

	w.lastArt.accents = []color.RGBA{
		{R: 0, G: 0, B: 0, A: 255}, {R: 255, G: 255, B: 255, A: 255},
	}
	w.track.Duration = 0
	if got := w.liveAccent(time.Now()); got != w.accent {
		t.Fatalf("unknown track length: want the static accent, got %v", got)
	}
}

// Once both a journey and a known duration exist, liveAccent must track the
// playhead - not the wall clock - so replaying the same spot in the song
// always gives back the exact same colour.
func TestLiveAccentTracksPositionNotTheWallClock(t *testing.T) {
	w := newTestPanel()
	w.accent = color.RGBA{R: 10, G: 10, B: 10, A: 255}
	w.lastArt = &artwork{accent: w.accent, accents: []color.RGBA{
		{R: 0, G: 0, B: 0, A: 255}, {R: 200, G: 0, B: 0, A: 255},
	}}
	w.track = LyricsTrack{
		Duration: time.Minute, Position: 30 * time.Second, PositionAt: time.Now(),
	}

	a := w.liveAccent(time.Now())
	time.Sleep(5 * time.Millisecond)
	w.track.PositionAt = time.Now() // same reported Position, later wall-clock reading
	b := w.liveAccent(time.Now())
	if a != b {
		t.Fatalf("same playhead position should give the same colour regardless of when it is asked, got %v then %v", a, b)
	}
}
