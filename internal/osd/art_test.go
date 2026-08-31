package osd

import (
	"image"
	"image/color"
	"testing"
)

// accentJourney must sweep left to right: a cover split cleanly into a red
// half and a blue half should come back with stops that start red and end
// blue, not some blend that erases which side was which.
func TestAccentJourneySweepsLeftToRight(t *testing.T) {
	const w, h = 240, 60
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 220, G: 40, B: 40, A: 255}
	blue := color.RGBA{R: 40, G: 40, B: 220, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				src.SetRGBA(x, y, red)
			} else {
				src.SetRGBA(x, y, blue)
			}
		}
	}

	stops := accentJourney(src)
	if len(stops) < 2 {
		t.Fatalf("want several stops, got %d", len(stops))
	}
	first, last := stops[0], stops[len(stops)-1]
	if first.R <= first.B {
		t.Fatalf("want the first stop to read red (from the left half), got %v", first)
	}
	if last.B <= last.R {
		t.Fatalf("want the last stop to read blue (from the right half), got %v", last)
	}
}

func TestAccentJourneyRefusesAnEmptyImage(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 0, 0))
	if got := accentJourney(src); got != nil {
		t.Fatalf("a zero-width source has nothing to sweep across, want nil, got %v", got)
	}
}
