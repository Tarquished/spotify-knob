package osd

import (
	"image"
	"testing"
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
