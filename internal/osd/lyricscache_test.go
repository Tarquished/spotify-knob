package osd

import (
	"image"
	"image/color"
	"testing"
)

func TestBuildCardGradientSizesAndOrientation(t *testing.T) {
	img := buildCardGradient(50, 100)
	if b := img.Bounds(); b.Dx() != 50 || b.Dy() != 100 {
		t.Fatalf("want 50x100, got %dx%d", b.Dx(), b.Dy())
	}
	top := img.RGBAAt(10, 0)
	bottom := img.RGBAAt(10, 99)
	if top == bottom {
		t.Fatal("want the gradient to actually change between top and bottom")
	}
	// colCardTop is the lighter of the two (see theme colours), so its
	// contribution at the top should read brighter than the bottom's.
	if top.R < bottom.R {
		t.Fatalf("want the top row brighter than the bottom, got top=%v bottom=%v", top, bottom)
	}
}

func TestBuildCardGradientDegenerateSizes(t *testing.T) {
	if img := buildCardGradient(0, 0); img == nil {
		t.Fatal("want a (possibly empty) image back, not nil")
	}
	img := buildCardGradient(10, 1) // must not divide by zero on a 1px-tall tile
	if b := img.Bounds(); b.Dy() != 1 {
		t.Fatalf("want a 1px tall image, got %d", b.Dy())
	}
}

func TestApplyScrimDarkensInPlace(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	before := img.RGBAAt(1, 1)
	applyScrim(img, rgba(0, 0, 0, 0.5))
	after := img.RGBAAt(1, 1)
	if after.R >= before.R {
		t.Fatalf("scrim should darken the pixel, got %v -> %v", before, after)
	}
	if after.A != 255 {
		t.Fatalf("scrim over an opaque pixel should stay opaque, got A=%d", after.A)
	}
}

func TestApplyScrimIgnoresNil(t *testing.T) {
	applyScrim(nil, rgba(0, 0, 0, 0.5)) // must not panic
}

// radialFalloffMask must peak at the centre and fall to nothing at the edge,
// matching painter.radial's own squared falloff.
func TestRadialFalloffMaskShape(t *testing.T) {
	bounds := image.Rect(0, 0, 41, 41)
	m := radialFalloffMask(20, 20, 20, bounds)
	centre := m.AlphaAt(20, 20).A
	edge := m.AlphaAt(0, 20).A
	mid := m.AlphaAt(10, 20).A

	if centre < 250 {
		t.Fatalf("want the centre near full alpha, got %d", centre)
	}
	if edge > 5 {
		t.Fatalf("want the edge near zero alpha, got %d", edge)
	}
	if !(centre > mid && mid > edge) {
		t.Fatalf("want a falloff from centre to edge, got centre=%d mid=%d edge=%d", centre, mid, edge)
	}
}

func TestRadialFalloffMaskRefusesZeroRadius(t *testing.T) {
	m := radialFalloffMask(5, 5, 0, image.Rect(0, 0, 10, 10))
	if m == nil {
		t.Fatal("want an (empty) mask back, not nil")
	}
}

// clipToRoundRect is the fix for a real bug: the header's glow used to bleed
// straight past the card's rounded corner into the transparent margin
// because the cached mask only knew its own circular falloff, not the
// card's shape. A pixel outside the card entirely must be cleared, a pixel
// inside the straight edges must survive, and a pixel in a rounded corner's
// cut-off triangle must be cleared even though it is inside the bounding box.
func TestClipToRoundRectRemovesTheCorners(t *testing.T) {
	m := image.NewAlpha(image.Rect(0, 0, 100, 100))
	for i := range m.Pix {
		m.Pix[i] = 255
	}
	clipToRoundRect(m, 10, 10, 60, 60, 15) // card at (10,10)-(70,70), 15px corners

	if got := m.AlphaAt(5, 5).A; got != 0 {
		t.Fatalf("outside the card entirely should be cleared, got %d", got)
	}
	if got := m.AlphaAt(40, 40).A; got != 255 {
		t.Fatalf("dead centre of the card should survive, got %d", got)
	}
	if got := m.AlphaAt(40, 12).A; got != 255 {
		t.Fatalf("the middle of a straight top edge should survive, got %d", got)
	}
	if got := m.AlphaAt(11, 11).A; got != 0 {
		t.Fatalf("the very corner pixel, well outside the rounded curve, should be cleared, got %d", got)
	}
}

func TestInsideRoundRectMatchesACircleAtTheCorner(t *testing.T) {
	// The top-left corner's own centre sits at (x0+rad, y0+rad) = (25,25).
	// A point diagonally 10px out from it, still inside both the x and y
	// corner bands, is within a 15px-radius curve; one diagonally 20px out
	// is past it - the classic corner-cutoff a plain box test would miss.
	// Offsets point back toward the actual corner tip at (10,10), which is
	// what keeps the sample inside the band the corner check even applies to
	// (px<x0+rad && py<y0+rad); outside that band any point is automatically
	// "inside" via the straight-edge fast path, which is not what this test
	// wants to exercise.
	const s = 0.70710678 // 1/sqrt(2), so the offset's straight-line length is exact
	if !insideRoundRect(25-10*s, 25-10*s, 10, 10, 100, 100, 15) {
		t.Fatal("10px diagonally from the corner centre should be inside a 15px radius")
	}
	if insideRoundRect(25-20*s, 25-20*s, 10, 10, 100, 100, 15) {
		t.Fatal("20px diagonally from the corner centre should be outside a 15px radius")
	}
}

// The chrome masks must actually be cached: asking for the same geometry
// twice should return the identical pointer, not rebuild it.
func TestShadowRingMaskIsCached(t *testing.T) {
	w := newTestPanel()
	w.paint = newPainter(500, 600)

	a := w.shadowRingMask(2, 5, 5, 400, 500, 20)
	b := w.shadowRingMask(2, 5, 5, 400, 500, 20)
	if a != b {
		t.Fatal("identical geometry should reuse the cached mask")
	}
	c := w.shadowRingMask(2, 5, 5, 401, 500, 20) // width changed
	if c == a {
		t.Fatal("changed geometry should rebuild the mask")
	}
}

func TestCardBackgroundSwitchesBetweenFlatAndAmbient(t *testing.T) {
	w := newTestPanel()
	w.paint = newPainter(500, 600)

	flat := w.cardBackground(400, 500)
	if flat == nil {
		t.Fatal("want a background image even with no art loaded")
	}

	w.ambientOn = true
	w.artURL = "http://example/cover.jpg"
	w.lastArt = &artwork{img: image.NewRGBA(image.Rect(0, 0, 46, 46))}
	amb := w.cardBackground(400, 500)
	if amb == flat {
		t.Fatal("turning ambient mode on should not reuse the flat gradient")
	}

	again := w.cardBackground(400, 500)
	if again != amb {
		t.Fatal("the same ambient state should reuse its cached image")
	}
}
