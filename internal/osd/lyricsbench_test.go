package osd

import (
	"image/color"
	"testing"
	"time"
)

// panelForBench is a full-size panel with a real multi-line synced doc, the
// worst realistic case for render() now that lyrics draw word by word
// instead of one string per row: an active line mid-song, wiping.
func panelForBench() *LyricsWindow {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.paint = newPainter(int(w.px(lyrMaxW)), int(w.px(lyrMaxH)))
	w.track = LyricsTrack{
		Title: "Weird Fishes / Arpeggi", Artist: "Radiohead",
		Duration: 5 * time.Minute, Position: 90 * time.Second,
		PositionAt: time.Now(), Playing: true,
	}
	w.accent = color.RGBA{R: 226, G: 88, B: 54, A: 255}
	w.lastArt = &artwork{img: nil, accent: w.accent}
	lines := []LyricLine{
		{At: 80 * time.Second, Text: "In the deepest ocean, the bottom of the sea"},
		{At: 84 * time.Second, Text: "Your eyes, they turn me, why should I stay here?"},
		{At: 88 * time.Second, Text: "I'd rather be an ape, than to see you like this"},
		{At: 92 * time.Second, Text: "There is nothing to explain"},
		{At: 96 * time.Second, Text: ""},
		{At: 100 * time.Second, Text: "The chains around my feet, will lift with the tide"},
		{At: 104 * time.Second, Text: "And a hundred thousand people, they can't be wrong"},
		{At: 108 * time.Second, Text: "It's a wonder we're alive tonight, alone"},
		{At: 112 * time.Second, Text: "I'll be no one's dog"},
	}
	w.doc = &LyricDoc{Synced: true, Source: "LRCLIB", Lines: lines}
	w.docKey = w.track.URI
	w.state = docReady
	w.pulsePeriod = pulsePeriodFor(lines)
	w.layout()
	w.active = 2
	return w
}

func BenchmarkLyricsRender(b *testing.B) {
	w := panelForBench()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.render()
	}
}

// The active line is where every extra draw call lives - the wipe redraws
// each word in it twice - so scrubbing through several words a run is closer
// to a worst case than sitting on one.
func BenchmarkLyricsRenderScrubbingWord(b *testing.B) {
	w := panelForBench()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.track.Position = 88*time.Second + time.Duration(i%3000)*time.Millisecond/1000
		w.track.PositionAt = time.Now()
		w.render()
	}
}

func BenchmarkLyricsRenderAmbient(b *testing.B) {
	w := panelForBench()
	w.ambientOn = true
	w.lastArt.img = nil // exercises the flat-gradient fallback path cheaply
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.render()
	}
}
