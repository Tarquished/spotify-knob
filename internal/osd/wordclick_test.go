package osd

import (
	"testing"
	"time"
)

// timeWords must spread a line proportionally to word length: a longer word
// takes longer to "sing" than a short one, and the whole run covers exactly
// the window it was given with no gaps or overlaps.
func TestTimeWordsSpreadsProportionally(t *testing.T) {
	spans := timeWords([]string{"hi", "wonderful"}, 10*time.Second, 6*time.Second)
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	if spans[0].at != 10*time.Second {
		t.Fatalf("first word should start at the line's own time, got %v", spans[0].at)
	}
	if spans[1].at != spans[0].at+spans[0].dur {
		t.Fatalf("second word should start exactly where the first ends: %v vs %v",
			spans[1].at, spans[0].at+spans[0].dur)
	}
	if end := spans[1].at + spans[1].dur; end != 16*time.Second {
		t.Fatalf("the run should cover the whole window, ended at %v", end)
	}
	// "wonderful" (9 runes) should take noticeably longer than "hi" (2 runes).
	if spans[1].dur <= spans[0].dur {
		t.Fatalf("want the longer word to take longer, got %v vs %v", spans[1].dur, spans[0].dur)
	}
}

func TestTimeWordsHandlesOneWord(t *testing.T) {
	spans := timeWords([]string{"solo"}, 5*time.Second, 2*time.Second)
	if len(spans) != 1 || spans[0].at != 5*time.Second || spans[0].dur != 2*time.Second {
		t.Fatalf("got %+v", spans)
	}
}

func TestTimeWordsWithNoDurationStillReturnsSomething(t *testing.T) {
	spans := timeWords([]string{"a", "b"}, 3*time.Second, 0)
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	for _, s := range spans {
		if s.at != 3*time.Second || s.dur != 0 {
			t.Fatalf("with no window, words should just sit at the line's time: %+v", s)
		}
	}
}

func TestTimeWordsEmptyInput(t *testing.T) {
	if got := timeWords(nil, time.Second, time.Second); len(got) != 0 {
		t.Fatalf("want no spans for no words, got %v", got)
	}
}

// wrapWords must place words left to right without overlap, wrap to a new
// row once a word would not fit, and never split a word across rows.
func TestWrapWordsPacksAndWraps(t *testing.T) {
	w := newTestPanel()
	face := w.fonts.face(semibold, 16)
	spaceW := measure(face, " ")

	words := timeWords([]string{"one", "two", "three", "four", "five"}, 0, 5*time.Second)
	rows := wrapWords(face, words, 60, spaceW) // narrow: forces several wraps

	if len(rows) < 2 {
		t.Fatalf("want at least 2 rows at this width, got %d", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		right := 0.0
		for i, wd := range row.words {
			if wd.x < right-0.01 {
				t.Fatalf("word %q overlaps the one before it in its row", wd.text)
			}
			right = wd.x + wd.w
			seen[wd.text] = true
			_ = i
		}
	}
	for _, want := range []string{"one", "two", "three", "four", "five"} {
		if !seen[want] {
			t.Fatalf("word %q went missing during wrap", want)
		}
	}
}

// A single word wider than the row is placed on its own row rather than
// being split - splitting it would break the one-word-one-click contract.
func TestWrapWordsNeverSplitsAWord(t *testing.T) {
	w := newTestPanel()
	face := w.fonts.face(semibold, 16)
	spaceW := measure(face, " ")

	words := timeWords([]string{"floccinaucinihilipilification"}, 0, time.Second)
	rows := wrapWords(face, words, 10, spaceW) // absurdly narrow
	if len(rows) != 1 || len(rows[0].words) != 1 {
		t.Fatalf("want the one long word alone on its own row, got %+v", rows)
	}
	if rows[0].words[0].text != "floccinaucinihilipilification" {
		t.Fatalf("the word must survive intact, got %q", rows[0].words[0].text)
	}
}

func TestWrapWordsEmptyInput(t *testing.T) {
	w := newTestPanel()
	face := w.fonts.face(semibold, 16)
	if got := wrapWords(face, nil, 200, measure(face, " ")); got != nil {
		t.Fatalf("want nil for no words, got %v", got)
	}
}

// estimateLineDur must never let a short line's words stretch across a long
// gap (an instrumental break before the next line), and never compress so
// far that words would blur together.
func TestEstimateLineDurClampsBothWays(t *testing.T) {
	w := newTestPanel()

	longGap := []LyricLine{{At: 0, Text: "one word"}, {At: 30 * time.Second, Text: "next"}}
	if d := w.estimateLineDur(longGap, 0); d > 6*time.Second {
		t.Fatalf("a long gap should be capped, got %v", d)
	}

	tightGap := []LyricLine{
		{At: 0, Text: "a lot of words crammed in here somehow"},
		{At: 200 * time.Millisecond, Text: "next"},
	}
	wc := 8 // matches the line above
	if d := w.estimateLineDur(tightGap, 0); d < time.Duration(wc)*150*time.Millisecond {
		t.Fatalf("a tight gap should be floored, got %v", d)
	}

	// The last line, with a known track duration, spreads across whatever is
	// left rather than the 6s fallback.
	w.track.Duration = 5 * time.Second
	last := []LyricLine{{At: 4 * time.Second, Text: "the end"}}
	if d := w.estimateLineDur(last, 0); d != time.Second {
		t.Fatalf("want 1s remaining to the track's end, got %v", d)
	}
}

// panelWithSingleLine is a minimal synced doc laid out at a fixed size, for
// exercising wordAt / click-to-seek without a real window.
func panelWithSingleLine(text string, at time.Duration, trackDur time.Duration) *LyricsWindow {
	w := newTestPanel()
	w.win = &lyricsWin{w: 430, h: 540}
	w.track = LyricsTrack{URI: "spotify:track:x", Duration: trackDur}
	w.doc = &LyricDoc{Synced: true, Source: "LRCLIB", Lines: []LyricLine{{At: at, Text: text}}}
	w.docKey = w.track.URI
	w.state = docReady
	w.layout()
	return w
}

// Clicking dead centre of a word must find that exact word.
func TestWordAtFindsTheClickedWord(t *testing.T) {
	w := panelWithSingleLine("hello little world", 2*time.Second, 4*time.Minute)
	if len(w.para) != 1 || len(w.para[0].rows) == 0 {
		t.Fatal("layout did not produce a row to click")
	}
	row := w.para[0].rows[0]
	if len(row.words) != 3 {
		t.Fatalf("want 3 words, got %d", len(row.words))
	}

	bx, by, _, _ := w.bodyRect()
	target := row.words[1] // "little"
	x := int(bx + target.x + target.w/2)
	y := int(by + w.para[0].top + w.lineH*0.5)

	got, ok := w.wordAt(x, y)
	if !ok {
		t.Fatal("expected a word hit")
	}
	if got.text != "little" {
		t.Fatalf("want 'little', got %q", got.text)
	}
}

// A click well to the right of the last word, still within the row's
// vertical band, must miss - it is empty space, not a fourth word.
func TestWordAtMissesEmptySpace(t *testing.T) {
	w := panelWithSingleLine("short line", time.Second, 4*time.Minute)
	bx, by, bw, _ := w.bodyRect()
	x := int(bx + bw - 2)
	y := int(by + w.para[0].top + w.lineH*0.5)
	if _, ok := w.wordAt(x, y); ok {
		t.Fatal("a click in the empty tail of the row should not hit anything")
	}
}

func TestWordAtMissesUnsyncedLyrics(t *testing.T) {
	w := panelWithSingleLine("hello world", time.Second, 4*time.Minute)
	w.doc.Synced = false
	w.wrapKey = ""
	w.layout()
	bx, by, _, _ := w.bodyRect()
	x := int(bx + 5)
	y := int(by + w.para[0].top + w.lineH*0.5)
	if _, ok := w.wordAt(x, y); ok {
		t.Fatal("unsynced lyrics have no timestamp to seek any word to")
	}
}

// End to end: clicking a word seeks to that word's own estimated moment, not
// the line's start.
func TestClickingAWordSeeksToItsOwnMoment(t *testing.T) {
	w := panelWithSingleLine("hello little world", 2*time.Second, 4*time.Minute)
	seen := make(chan time.Duration, 1)
	w.opts.OnSeek = func(p time.Duration) { seen <- p }
	w.visibl.Store(true)
	w.win.visible = true

	row := w.para[0].rows[0]
	target := row.words[2] // "world" - the last word, well after the line's own start
	bx, by, _, _ := w.bodyRect()
	x := int(bx + target.x + target.w/2)
	y := int(by + w.para[0].top + w.lineH*0.5)

	w.onMouseDown(x, y)

	select {
	case got := <-seen:
		if got != target.at {
			t.Fatalf("want seek to %v (the word's own moment), got %v", target.at, got)
		}
		if got <= 2*time.Second {
			t.Fatalf("the last word of the line should seek past the line's own start (2s), got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnSeek was never called")
	}
}

// pulsePeriodFor must refuse to invent a rate from too little data, and must
// clamp an honest one into a visually sane range.
func TestPulsePeriodForNeedsEnoughData(t *testing.T) {
	if got := pulsePeriodFor(nil); got != 0 {
		t.Fatalf("no lines: want 0, got %v", got)
	}
	few := []LyricLine{{At: 0}, {At: time.Second}, {At: 2 * time.Second}}
	if got := pulsePeriodFor(few); got != 0 {
		t.Fatalf("only 2 gaps: want 0, got %v", got)
	}
}

func TestPulsePeriodForClampsToASaneRange(t *testing.T) {
	fast := []LyricLine{{At: 0}, {At: 100 * time.Millisecond}, {At: 200 * time.Millisecond}, {At: 300 * time.Millisecond}}
	if got := pulsePeriodFor(fast); got != 700*time.Millisecond {
		t.Fatalf("want the floor (700ms), got %v", got)
	}

	slow := []LyricLine{{At: 0}, {At: 10 * time.Second}, {At: 20 * time.Second}, {At: 30 * time.Second}}
	if got := pulsePeriodFor(slow); got != 2400*time.Millisecond {
		t.Fatalf("want the ceiling (2.4s), got %v", got)
	}

	normal := []LyricLine{{At: 0}, {At: 1500 * time.Millisecond}, {At: 3 * time.Second}, {At: 4500 * time.Millisecond}}
	if got := pulsePeriodFor(normal); got != 1500*time.Millisecond {
		t.Fatalf("want the median gap (1.5s), got %v", got)
	}
}

// The pulse follows playback position, not the wall clock: a paused track
// (position not advancing) must hold the same breath value indefinitely.
func TestPulseBreathFollowsPositionNotTheClock(t *testing.T) {
	period := time.Second
	a := pulseBreath(300*time.Millisecond, period)
	b := pulseBreath(300*time.Millisecond, period)
	if a != b {
		t.Fatalf("the same position should always give the same breath, got %v and %v", a, b)
	}
	if got := pulseBreath(500*time.Millisecond, period); got < 0 || got > 1 {
		t.Fatalf("breath should stay within 0..1, got %v", got)
	}
	if got := pulseBreath(0, period); got != 1 {
		t.Fatalf("at the start of a cycle the breath should peak at 1, got %v", got)
	}
}

func TestPulseBreathWithNoPeriodIsZero(t *testing.T) {
	if got := pulseBreath(time.Second, 0); got != 0 {
		t.Fatalf("no period should give no breath, got %v", got)
	}
}
