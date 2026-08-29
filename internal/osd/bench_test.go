package osd

import (
	"image/color"
	"testing"
)

func benchState(o *OSD, art *artwork) frameState {
	return frameState{
		kind: KindVolume, volume: 45, bar: 0.45,
		title: "Weird Fishes / Arpeggi", artist: "Radiohead",
		art: art, accent: art.accent, artFade: 1,
	}
}

func BenchmarkRenderFrame(b *testing.B) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	art := &artwork{img: nil, accent: color.RGBA{R: 226, G: 88, B: 54, A: 255}}
	st := benchState(o, art)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o.renderFrame(st)
	}
}

// Worst case: the bar is gliding, so its blur mask changes every frame.
func BenchmarkRenderFrameMoving(b *testing.B) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	art := &artwork{img: nil, accent: color.RGBA{R: 226, G: 88, B: 54, A: 255}}
	st := benchState(o, art)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st.bar = float64(i%100) / 100
		o.renderFrame(st)
	}
}
