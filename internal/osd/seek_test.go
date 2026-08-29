package osd

import (
	"math"
	"testing"
	"time"
)

// newTestPanel is a panel with enough state to exercise its logic, but no
// window: none of what is tested here paints anything.
func newTestPanel() *LyricsWindow {
	return &LyricsWindow{
		log:    testLogger(),
		events: make(chan lyricsCmd, 8),
		art:    newArtCache(46, 46),
		scale:  1,
		active: -1,
	}
}

// seekTarget is the whole of the rail's arithmetic, so it carries the whole of
// the rail's test.
func TestSeekTargetMapsAlongTheRail(t *testing.T) {
	const railX, railW = 100.0, 200.0
	dur := 4 * time.Minute

	cases := []struct {
		name string
		x    float64
		want time.Duration
	}{
		{"left end", 100, 0},
		{"quarter", 150, time.Minute},
		{"middle", 200, 2 * time.Minute},
		{"right end", 300, 4 * time.Minute},
		{"past the left end clamps", 20, 0},
		{"past the right end clamps", 900, 4 * time.Minute},
	}
	for _, c := range cases {
		if got := seekTarget(c.x, railX, railW, dur); got != c.want {
			t.Errorf("%s: seekTarget(%v) = %v, want %v", c.name, c.x, got, c.want)
		}
	}
}

// A track of unknown length has nowhere to seek to, and a zero-width rail
// would divide by zero.
func TestSeekTargetRefusesNonsense(t *testing.T) {
	if got := seekTarget(150, 100, 200, 0); got != 0 {
		t.Errorf("no duration should give no target, got %v", got)
	}
	if got := seekTarget(150, 100, 0, time.Minute); got != 0 {
		t.Errorf("no rail should give no target, got %v", got)
	}
}

// While the handle is held the panel reports the handle's position, not the
// music's - that is what makes the highlighted lyric follow the scrub.
func TestPositionFollowsTheScrub(t *testing.T) {
	w := &LyricsWindow{}
	w.track = LyricsTrack{
		Duration: 4 * time.Minute, Position: 10 * time.Second,
		PositionAt: time.Now(), Playing: true,
	}

	if got := w.position(time.Now()); got < 9*time.Second || got > 12*time.Second {
		t.Fatalf("want roughly the real playhead, got %v", got)
	}

	w.scrubbing = true
	w.scrubPos = 2 * time.Minute
	if got := w.position(time.Now()); got != 2*time.Minute {
		t.Fatalf("while scrubbing the handle wins, got %v", got)
	}

	w.scrubbing = false
	if got := w.position(time.Now()); got > 12*time.Second {
		t.Fatalf("letting go should return to the real playhead, got %v", got)
	}
}

// The regression that makes seeking feel solid: a poll carrying the position
// from before the seek must not drag the rail back under the cursor.
func TestSetTrackDoesNotUndoAFreshSeek(t *testing.T) {
	w := newTestPanel()
	w.track = LyricsTrack{URI: "u", Title: "T", Duration: 4 * time.Minute}

	w.scrubbing = true
	w.scrubPos = 3 * time.Minute
	w.commitSeek()

	if w.scrubbing {
		t.Fatal("letting go should end the scrub")
	}
	if w.track.Position != 3*time.Minute {
		t.Fatalf("the dropped position should be adopted, got %v", w.track.Position)
	}

	// A poll that was in flight during the seek reports the old playhead.
	w.setTrack(nil, LyricsTrack{
		URI: "u", Title: "T", Duration: 4 * time.Minute,
		Position: 20 * time.Second, PositionAt: time.Now(),
	})
	if w.track.Position != 3*time.Minute {
		t.Fatalf("a stale reading must not win, got %v", w.track.Position)
	}

	// Once the hold expires the daemon is authoritative again.
	w.seekHold = time.Now().Add(-time.Second)
	w.setTrack(nil, LyricsTrack{
		URI: "u", Title: "T", Duration: 4 * time.Minute,
		Position: 25 * time.Second, PositionAt: time.Now(),
	})
	if w.track.Position != 25*time.Second {
		t.Fatalf("after the hold the reading should win, got %v", w.track.Position)
	}
}

// A different song is a different timeline; the hold must not carry over.
func TestSeekHoldDoesNotCrossTracks(t *testing.T) {
	w := newTestPanel()
	w.track = LyricsTrack{URI: "u1", Title: "One", Duration: 4 * time.Minute}
	w.scrubbing = true
	w.scrubPos = 3 * time.Minute
	w.commitSeek()

	w.setTrack(nil, LyricsTrack{
		URI: "u2", Title: "Two", Duration: 3 * time.Minute,
		Position: 5 * time.Second, PositionAt: time.Now(),
	})
	if w.track.Position != 5*time.Second {
		t.Fatalf("a new track starts where it says it does, got %v", w.track.Position)
	}
}

// commitSeek reports the position exactly once, and only when there was a
// scrub to commit.
func TestCommitSeekReportsOnce(t *testing.T) {
	seen := make(chan time.Duration, 4)
	w := newTestPanel()
	w.opts.OnSeek = func(p time.Duration) { seen <- p }
	w.track = LyricsTrack{URI: "u", Title: "T", Duration: 4 * time.Minute}

	w.commitSeek() // nothing was being dragged
	w.scrubbing = true
	w.scrubPos = 90 * time.Second
	w.commitSeek()
	w.commitSeek() // a second release changes nothing

	select {
	case got := <-seen:
		if got != 90*time.Second {
			t.Fatalf("want 1m30s, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the seek was never reported")
	}
	select {
	case got := <-seen:
		t.Fatalf("the seek was reported twice (%v)", got)
	case <-time.After(150 * time.Millisecond):
	}
}

// The opacity slider's arithmetic, and its inverse. Anything below the floor
// stops being readable over a bright background, so the mapping never offers
// it - not from the slider and not from a config file.
func TestOpacityMapsAlongTheSlider(t *testing.T) {
	const x0, w0 = 200.0, 80.0

	if got := opacityAt(x0, x0, w0); got != lyrOpacityMin {
		t.Errorf("left end should be the floor, got %v", got)
	}
	if got := opacityAt(x0+w0, x0, w0); got != 1 {
		t.Errorf("right end should be fully opaque, got %v", got)
	}
	mid := opacityAt(x0+w0/2, x0, w0)
	if want := lyrOpacityMin + (1-lyrOpacityMin)/2; math.Abs(mid-want) > 1e-9 {
		t.Errorf("middle: want %v, got %v", want, mid)
	}
	if got := opacityAt(x0-500, x0, w0); got != lyrOpacityMin {
		t.Errorf("past the left end should clamp, got %v", got)
	}
	if got := opacityAt(x0+900, x0, w0); got != 1 {
		t.Errorf("past the right end should clamp, got %v", got)
	}

	// Round-tripping a value through the drawing fraction must land back on
	// itself, or the handle would sit somewhere other than the setting.
	for _, v := range []float64{lyrOpacityMin, 0.5, 0.7, 0.94, 1} {
		f := opacityFraction(v)
		if back := opacityAt(x0+f*w0, x0, w0); math.Abs(back-v) > 1e-9 {
			t.Errorf("round trip of %v gave %v", v, back)
		}
	}
}

func TestClampOpacityRefusesTheUnreadable(t *testing.T) {
	if got := clampOpacity(0.05); got != lyrOpacityMin {
		t.Errorf("want the floor, got %v", got)
	}
	if got := clampOpacity(2); got != 1 {
		t.Errorf("want 1, got %v", got)
	}
	if got := clampOpacity(0.8); got != 0.8 {
		t.Errorf("a legal value should pass through, got %v", got)
	}
}

// A zero opacity in the options means "not set", and must not be read as
// "invisible".
func TestUnsetOpacityFallsBackToADefault(t *testing.T) {
	w := &LyricsWindow{}
	if got := w.opacity(); got < 0.9 {
		t.Fatalf("an unset opacity should default to nearly solid, got %v", got)
	}
	w.opts.Opacity = 0.6
	if got := w.opacity(); got != 0.6 {
		t.Fatalf("want 0.6, got %v", got)
	}
}
