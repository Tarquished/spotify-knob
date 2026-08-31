package osd

import (
	"testing"
	"time"
)

// resume is what open() runs before ever presenting a frame: it must land
// exactly where the song already is, not wherever the panel was scrolled to
// when it was last closed.
func TestResumeSnapsStraightToTheCurrentLineWithoutEasing(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.paint = newPainter(int(w.px(lyrMaxW)), int(w.px(lyrMaxH)))
	w.track = LyricsTrack{
		URI: "u", Title: "T", Duration: 4 * time.Minute,
		Position: 90 * time.Second, PositionAt: time.Now(), Playing: true,
	}
	lines := []LyricLine{
		{At: 0, Text: "one"},
		{At: 30 * time.Second, Text: "two"},
		{At: 88 * time.Second, Text: "three"},
		{At: 150 * time.Second, Text: "four"},
	}
	w.doc = &LyricDoc{Synced: true, Source: "LRCLIB", Lines: lines}
	w.docKey = w.track.URI
	w.state = docReady

	// Simulate the panel having been left scrolled somewhere else entirely
	// while it sat hidden - the state advance() would have left behind if it
	// still ran, which it does not (see Run).
	w.layout()
	w.active = 0
	w.scroll, w.scrollTo = 0, 0

	w.resume(time.Now())

	if w.active != 2 {
		t.Fatalf("want line 2 (\"three\") active at 90s, got %d", w.active)
	}
	wantScroll := w.clampScroll(w.anchorFor(2))
	if w.scroll != wantScroll || w.scrollTo != wantScroll {
		t.Fatalf("want the scroll snapped straight to %v, got scroll=%v scrollTo=%v",
			wantScroll, w.scroll, w.scrollTo)
	}
	if w.openedAt.IsZero() {
		t.Fatal("want openedAt set, so the body can still fade in")
	}
}

// Whatever was active right before the panel closed must not be treated as
// something the newly-resumed line just pushed away - it never visibly held
// that position on screen for the user to see leave it.
func TestResumeClearsAnyPendingPushAnimation(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.paint = newPainter(int(w.px(lyrMaxW)), int(w.px(lyrMaxH)))
	w.track = LyricsTrack{URI: "u", Title: "T", Duration: time.Minute}
	w.doc = &LyricDoc{Synced: true, Source: "LRCLIB", Lines: []LyricLine{{At: 0, Text: "one"}}}
	w.docKey = w.track.URI
	w.state = docReady
	w.layout()

	w.prevActive, w.prevActiveAt = 3, time.Now()
	w.resume(time.Now())

	if w.prevActive != -1 {
		t.Fatalf("want prevActive cleared, got %d", w.prevActive)
	}
	if !w.prevActiveAt.IsZero() {
		t.Fatal("want prevActiveAt cleared alongside it")
	}
}

// advance keeps requesting repaints while the open-fade is still running,
// and stops the moment it is done - it must not keep the panel redrawing
// forever just because it was opened once.
func TestAdvanceStopsRequestingRepaintsOnceTheOpenFadeElapses(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.track = LyricsTrack{Duration: time.Minute}

	w.openedAt = time.Now().Add(-lyrOpenFade / 2) // still fading
	w.dirty = false
	w.advance(time.Now())
	if !w.dirty {
		t.Fatal("want a repaint requested while the open-fade is still running")
	}

	w.openedAt = time.Now().Add(-lyrOpenFade * 2) // long done
	w.dirty = false
	w.advance(time.Now())
	if w.dirty {
		t.Fatal("want no repaint requested purely for a fade that already finished")
	}
}

// When the active line changes, advance records what it changed from, so
// drawBody can animate that line settling into place - but only while
// something was actually active before; there is nothing to push away from
// "no line was highlighted yet".
func TestAdvanceRecordsThePreviousActiveLineOnChange(t *testing.T) {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.track = LyricsTrack{
		URI: "u", Duration: time.Minute,
		Position: 0, PositionAt: time.Now(), Playing: true,
	}
	lines := []LyricLine{
		{At: 0, Text: "one"},
		{At: 5 * time.Second, Text: "two"},
	}
	w.doc = &LyricDoc{Synced: true, Lines: lines}
	w.docKey = w.track.URI
	w.state = docReady
	w.layout()

	now := time.Now()
	w.advance(now) // active becomes 0; nothing was active before
	if w.active != 0 {
		t.Fatalf("want line 0 active, got %d", w.active)
	}
	if w.prevActive != -1 {
		t.Fatalf("want no previous line recorded on the very first activation, got %d", w.prevActive)
	}

	w.track.Position = 6 * time.Second
	w.track.PositionAt = now
	w.advance(now)
	if w.active != 1 {
		t.Fatalf("want line 1 active, got %d", w.active)
	}
	if w.prevActive != 0 {
		t.Fatalf("want line 0 recorded as the one just pushed away, got %d", w.prevActive)
	}
}

// inLongGap is the trigger for the instrumental background wash: it must
// fire for a real gap - before the first line, after the last one once the
// track's length is known, and between two lines timed far apart - and stay
// quiet for anything shorter, or for an unknown-length track's tail.
func TestInLongGapDetectsEachKindOfGap(t *testing.T) {
	w := newTestPanel()
	w.doc = &LyricDoc{Synced: true, Lines: []LyricLine{
		{At: 20 * time.Second, Text: "one"},
		{At: 24 * time.Second, Text: "two"},
		{At: 60 * time.Second, Text: "three"},
	}}

	if !w.inLongGap(5 * time.Second) {
		t.Error("want a long gap before the first line, 20s in")
	}
	if w.inLongGap(22 * time.Second) {
		t.Error("want no long gap between two lines only 4s apart")
	}
	if !w.inLongGap(30 * time.Second) {
		t.Error("want a long gap between lines 36s apart")
	}

	w.track.Duration = 0
	if w.inLongGap(90 * time.Second) {
		t.Error("want no gap claimed past the last line without a known track length")
	}
	w.track.Duration = 90 * time.Second
	if !w.inLongGap(80 * time.Second) {
		t.Error("want a long gap in the 30s outro once the track length is known")
	}
}

func TestInLongGapIgnoresUnsyncedOrEmptyDocs(t *testing.T) {
	w := newTestPanel()
	if w.inLongGap(time.Minute) {
		t.Error("no doc at all should never claim a gap")
	}
	w.doc = &LyricDoc{Synced: false, Lines: []LyricLine{{At: 0, Text: "x"}}}
	if w.inLongGap(time.Minute) {
		t.Error("unsynced lyrics have no timeline to be in a gap of")
	}
}
