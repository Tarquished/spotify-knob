package osd

import (
	"sync/atomic"
	"testing"
	"time"
)

// panelWithLyrics builds a panel with a synced doc, laid out at a fixed size.
// bodyRect, footerMetrics and headerMetrics only ever read win.w and win.h,
// so a bare *lyricsWin with those two fields set exercises the real geometry
// without creating an actual window.
func panelWithLyrics(lines []LyricLine) *LyricsWindow {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.track = LyricsTrack{URI: "spotify:track:abc", Duration: 4 * time.Minute}
	w.doc = &LyricDoc{Synced: true, Source: "LRCLIB", Lines: lines}
	w.docKey = w.track.URI
	w.state = docReady
	w.layout()
	return w
}

func TestLineAtFindsTheParagraphUnderThePoint(t *testing.T) {
	w := panelWithLyrics([]LyricLine{
		{At: 1 * time.Second, Text: "one"},
		{At: 2 * time.Second, Text: "two"},
		{At: 3 * time.Second, Text: "three"},
	})
	_, by, _, _ := w.bodyRect()

	if got := w.lineAt(int(by) + 2); got != 0 {
		t.Fatalf("want line 0 at the top of the body, got %d", got)
	}

	second := by + w.para[1].top
	if got := w.lineAt(int(second) + 2); got != 1 {
		t.Fatalf("want line 1, got %d", got)
	}

	if got := w.lineAt(int(by) - 100); got != -1 {
		t.Fatalf("above the body should hit nothing, got %d", got)
	}
}

func TestLineAtIgnoresUnsyncedLyrics(t *testing.T) {
	w := panelWithLyrics(nil)
	w.doc.Synced = false
	w.doc.Lines = []LyricLine{{Text: "plain, no timestamp"}}
	w.layout()

	_, by, _, _ := w.bodyRect()
	if got := w.lineAt(int(by) + 10); got != -1 {
		t.Fatalf("unsynced lyrics have nothing to seek to, got %d", got)
	}
}

func TestLineAtOnAnEmptyPanelDoesNotPanic(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	if got := w.lineAt(300); got != -1 {
		t.Fatalf("no doc at all should hit nothing, got %d", got)
	}
}

// Two clicks on the same line inside the window count as a double-click and
// seek there.
func TestHandleLineClickSeeksOnADoubleClick(t *testing.T) {
	w := panelWithLyrics([]LyricLine{
		{At: 1 * time.Second, Text: "one"},
		{At: 42 * time.Second, Text: "two"},
	})
	seenCh := make(chan time.Duration, 1)
	w.opts.OnSeek = func(p time.Duration) { seenCh <- p }

	_, by, _, _ := w.bodyRect()
	y := int(by+w.para[1].top) + 2
	t0 := time.Now()

	if w.handleLineClick(200, y, t0) {
		t.Fatal("a single click must not seek")
	}
	if !w.handleLineClick(200, y, t0.Add(150*time.Millisecond)) {
		t.Fatal("a second click on the same line within the window should seek")
	}

	select {
	case got := <-seenCh:
		if got != 42*time.Second {
			t.Fatalf("want 42s, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnSeek was never called")
	}
	if w.scrubbing {
		t.Fatal("a line double-click is not a rail scrub")
	}
}

// Two clicks on different lines are two single clicks, not a pair.
func TestHandleLineClickIgnoresTwoDifferentLines(t *testing.T) {
	w := panelWithLyrics([]LyricLine{
		{At: 1 * time.Second, Text: "one"},
		{At: 42 * time.Second, Text: "two"},
	})
	var seeks int32
	w.opts.OnSeek = func(time.Duration) { atomic.AddInt32(&seeks, 1) }

	_, by, _, _ := w.bodyRect()
	y0 := int(by+w.para[0].top) + 2
	y1 := int(by+w.para[1].top) + 2
	t0 := time.Now()

	if w.handleLineClick(200, y0, t0) {
		t.Fatal("first click must not seek")
	}
	if w.handleLineClick(200, y1, t0.Add(100*time.Millisecond)) {
		t.Fatal("a click on a different line is not a double-click")
	}

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&seeks); got != 0 {
		t.Fatalf("want 0 seeks, got %d", got)
	}
}

// Two clicks spaced further apart than the window are two single clicks.
func TestHandleLineClickIgnoresASlowSecondClick(t *testing.T) {
	w := panelWithLyrics([]LyricLine{{At: 5 * time.Second, Text: "only"}})
	var seeks int32
	w.opts.OnSeek = func(time.Duration) { atomic.AddInt32(&seeks, 1) }

	_, by, _, _ := w.bodyRect()
	y := int(by+w.para[0].top) + 2
	t0 := time.Now()

	w.handleLineClick(200, y, t0)
	if w.handleLineClick(200, y, t0.Add(lyrDoubleClickWindow+50*time.Millisecond)) {
		t.Fatal("a click after the double-click window should not seek")
	}

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&seeks); got != 0 {
		t.Fatalf("want 0 seeks, got %d", got)
	}
}

// A third click right behind a completed pair must not fire a second seek -
// the pair is consumed, and starts a fresh single click instead of chaining.
func TestHandleLineClickConsumesTheDoubleClick(t *testing.T) {
	w := panelWithLyrics([]LyricLine{
		{At: 5 * time.Second, Text: "one"},
		{At: 9 * time.Second, Text: "two"},
	})
	var seeks int32
	w.opts.OnSeek = func(time.Duration) { atomic.AddInt32(&seeks, 1) }

	_, by, _, _ := w.bodyRect()
	y := int(by+w.para[0].top) + 2
	t0 := time.Now()

	w.handleLineClick(200, y, t0)
	w.handleLineClick(200, y, t0.Add(100*time.Millisecond)) // the double-click
	w.handleLineClick(200, y, t0.Add(150*time.Millisecond)) // right behind it

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&seeks); got != 1 {
		t.Fatalf("want exactly one seek, got %d", got)
	}
}

// A track of unknown length has no rail and no double-click seek either -
// the same gate that hides the progress rail governs this.
func TestHandleLineClickRequiresAKnownDuration(t *testing.T) {
	w := panelWithLyrics([]LyricLine{{At: 5 * time.Second, Text: "one"}})
	w.track.Duration = 0
	var seeks int32
	w.opts.OnSeek = func(time.Duration) { atomic.AddInt32(&seeks, 1) }

	_, by, _, _ := w.bodyRect()
	y := int(by+w.para[0].top) + 2
	t0 := time.Now()
	w.handleLineClick(200, y, t0)
	w.handleLineClick(200, y, t0.Add(100*time.Millisecond))

	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&seeks); got != 0 {
		t.Fatalf("an unknown-length track should never seek, got %d", got)
	}
}

// A new track must not let a stale paragraph index pair up with a click on
// the song that replaced it.
func TestDocChangeClearsTheClickTracking(t *testing.T) {
	w := panelWithLyrics([]LyricLine{
		{At: 5 * time.Second, Text: "one"},
		{At: 9 * time.Second, Text: "two"},
	})
	var seeks int32
	w.opts.OnSeek = func(time.Duration) { atomic.AddInt32(&seeks, 1) }

	_, by, _, _ := w.bodyRect()
	y := int(by+w.para[0].top) + 2
	t0 := time.Now()
	w.handleLineClick(200, y, t0) // first half of what would be a pair

	w.apply(nil, lyricsCmd{
		kind: lcDoc, key: w.track.URI, state: docReady,
		doc: &LyricDoc{Synced: true, Lines: []LyricLine{
			{At: 1 * time.Second, Text: "different"},
			{At: 2 * time.Second, Text: "song"},
		}},
	})

	if w.handleLineClick(200, y, t0.Add(100*time.Millisecond)) {
		t.Fatal("a click on the new track must not pair with one from the old track")
	}
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&seeks); got != 0 {
		t.Fatalf("want 0 seeks, got %d", got)
	}
}

// openTrack must never call the callback for a track with no URI, and must
// never panic when no callback was ever wired up.
func TestOpenTrackWithNothingToOpen(t *testing.T) {
	w := panelWithLyrics(nil)
	w.track.URI = ""
	called := false
	w.opts.OnOpenSpotify = func(string) { called = true }
	w.openTrack()
	time.Sleep(30 * time.Millisecond)
	if called {
		t.Fatal("a track with no URI must not open anything")
	}

	w.track.URI = "spotify:track:abc"
	w.opts.OnOpenSpotify = nil
	w.openTrack() // must not panic
}

func TestOpenTrackPassesTheCurrentURI(t *testing.T) {
	w := panelWithLyrics(nil)
	w.track.URI = "spotify:track:xyz"
	got := make(chan string, 1)
	w.opts.OnOpenSpotify = func(uri string) { got <- uri }
	w.openTrack()

	select {
	case uri := <-got:
		if uri != "spotify:track:xyz" {
			t.Fatalf("want spotify:track:xyz, got %q", uri)
		}
	case <-time.After(time.Second):
		t.Fatal("OnOpenSpotify was never called")
	}
}

// The open icon hides itself, rather than sitting there doing nothing,
// when there is no track to open.
func TestOpenIconHiddenWithNoTrack(t *testing.T) {
	w := panelWithLyrics(nil)
	w.track.URI = ""
	_, closeY, closeR, openX, _, _, _ := w.headerMetrics()
	if got := w.hitZone(int(openX), int(closeY+closeR)); got == zoneOpenSpotify {
		t.Fatal("no URI should mean no icon to hit")
	}
}

// hitZone must agree with the icon's own geometry: a point inside the drawn
// circle is clickable, and a point well outside it is not mistaken for it.
func TestHitZoneMatchesTheOpenIcon(t *testing.T) {
	w := panelWithLyrics(nil) // sets a track URI
	closeX, closeY, closeR, openX, _, _, _ := w.headerMetrics()

	if got := w.hitZone(int(openX), int(closeY)); got != zoneOpenSpotify {
		t.Fatalf("the icon's own centre should hit zoneOpenSpotify, got %v", got)
	}
	if got := w.hitZone(int(openX-closeR*3), int(closeY)); got == zoneOpenSpotify {
		t.Fatal("well to the left of the icon should not hit it")
	}
	// The two icons sit close together on purpose; they still must not
	// answer for each other.
	if got := w.hitZone(int(closeX), int(closeY)); got != zoneClose {
		t.Fatalf("the close button's own centre should still hit zoneClose, got %v", got)
	}
}
