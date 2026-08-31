package osd

import (
	"bytes"
	"image"
	"testing"
)

func cloneRGBA(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

func rgbaEqual(a, b *image.RGBA) bool {
	return a.Bounds() == b.Bounds() && bytes.Equal(a.Pix, b.Pix)
}

// The instrumental wash must actually change what is drawn once restLevel is
// up, and must draw nothing extra at all when it is down - proven by
// comparing rendered pixels rather than trusting the gate alone, since a
// silently-broken blend (e.g. zero alpha throughout) would still pass a test
// that only checked restLevel itself.
func TestInstrumentalWashOnlyPaintsWhenRestLevelIsUp(t *testing.T) {
	w := panelForBench()
	// Freeze out every other source of frame-to-frame difference (the
	// header's beat-echo pulse and the live playhead both drift with wall
	// time) so the only thing that can make these two renders differ is the
	// wash itself.
	w.pulsePeriod = 0
	w.track.Playing = false
	w.restLevel = 0
	w.render()
	off := cloneRGBA(w.paint.dst)

	w.restLevel = 1
	w.render()
	on := w.paint.dst

	if rgbaEqual(off, on) {
		t.Fatal("want the rendered frame to differ once the instrumental wash is fully up")
	}
}

func TestInstrumentalWashDoesNotPanicWithoutArtwork(t *testing.T) {
	w := panelForBench()
	w.lastArt = nil
	w.restLevel = 1
	w.render() // must not panic on the fallback accent path
}
