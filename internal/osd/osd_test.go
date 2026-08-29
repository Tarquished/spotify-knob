package osd

import (
	"context"
	"testing"
	"time"
)

func testOSD() *OSD {
	// HideFullscreen off so the state machine can be exercised without
	// querying the shell.
	return New(Options{Enabled: true, Scale: 1, HideFullscreen: false}, testLogger())
}

// The card must walk enter -> hold -> exit -> gone, and report itself hidden
// only at the very end.
func TestCardLifecycle(t *testing.T) {
	o := testOSD()
	ctx := context.Background()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50}, ctx)
	if o.ph != entering {
		t.Fatalf("want entering after a volume event, got %v", o.ph)
	}

	// Mid-entry: partly faded in and still sliding.
	_, tr, visible := o.advance(o.phaseStart.Add(enterDur / 2))
	if !visible {
		t.Fatal("card should be visible mid-entry")
	}
	if tr.opacity <= 0 || tr.opacity >= 1 {
		t.Fatalf("opacity %.2f should be between 0 and 1 mid-entry", tr.opacity)
	}
	if tr.offsetY <= 0 {
		t.Fatal("card should still be sliding in")
	}

	_, tr, _ = o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	if o.ph != holding {
		t.Fatalf("want holding after the entry finishes, got %v", o.ph)
	}
	if tr.opacity != 1 || tr.offsetY != 0 {
		t.Fatalf("card should be fully settled while holding: %+v", tr)
	}

	o.advance(o.holdUntil.Add(time.Millisecond))
	if o.ph != exiting {
		t.Fatalf("want exiting once the hold expires, got %v", o.ph)
	}

	_, _, visible = o.advance(o.phaseStart.Add(exitDur + time.Millisecond))
	if visible {
		t.Fatal("card should report itself gone after the exit")
	}
	if o.ph != hidden {
		t.Fatalf("want hidden, got %v", o.ph)
	}
}

// Turning the knob again while the card is fading out must catch it, not
// restart it from nothing: the opacity may not drop on that frame.
func TestReshowDuringExitDoesNotFlash(t *testing.T) {
	o := testOSD()
	ctx := context.Background()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50}, ctx)
	o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	o.advance(o.holdUntil.Add(time.Millisecond))
	if o.ph != exiting {
		t.Fatalf("setup: want exiting, got %v", o.ph)
	}

	// Let it fade halfway out, then interrupt.
	_, half, _ := o.advance(o.phaseStart.Add(exitDur / 2))
	o.apply(event{typ: evVolume, kind: KindVolume, volume: 55}, ctx)
	if o.ph != entering {
		t.Fatalf("want entering after the interrupt, got %v", o.ph)
	}

	_, resumed, visible := o.advance(time.Now())
	if !visible {
		t.Fatal("card should still be visible after the interrupt")
	}
	if resumed.opacity < half.opacity-0.05 {
		t.Fatalf("opacity dropped on interrupt (%.2f -> %.2f), that is a visible flash",
			half.opacity, resumed.opacity)
	}
}

// The hold window has to be extended by each new turn, otherwise the card
// disappears mid-spin.
func TestEachTurnExtendsTheHold(t *testing.T) {
	o := testOSD()
	ctx := context.Background()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50}, ctx)
	first := o.holdUntil

	time.Sleep(10 * time.Millisecond)
	o.apply(event{typ: evVolume, kind: KindVolume, volume: 55}, ctx)
	if !o.holdUntil.After(first) {
		t.Fatal("a second turn did not push the hold out")
	}
	if o.content.volume != 55 {
		t.Fatalf("want volume 55, got %d", o.content.volume)
	}
}

// Artwork that lands after the card moved on must be ignored, or a stale
// cover flashes onto the wrong track.
func TestLateArtworkForAnotherTrackIsIgnored(t *testing.T) {
	o := testOSD()
	ctx := context.Background()

	o.apply(event{typ: evTrack, kind: KindTrack, track: Track{Title: "B"}}, ctx)
	o.wantURL = "current"

	stale := &artwork{accent: colAccentFallback}
	o.apply(event{typ: evArt, url: "previous", art: stale}, ctx)
	if o.content.art != nil {
		t.Fatal("artwork for a superseded track was applied")
	}

	fresh := &artwork{accent: colAccentFallback}
	o.apply(event{typ: evArt, url: "current", art: fresh}, ctx)
	if o.content.art != fresh {
		t.Fatal("artwork for the current track was not applied")
	}
}

// A full queue must never block the caller: these events come from the
// keyboard hook's path.
func TestPushNeverBlocks(t *testing.T) {
	o := testOSD()
	done := make(chan struct{})
	go func() {
		for i := 0; i < cap(o.events)*3; i++ {
			o.ShowVolume(i%100, Track{Title: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ShowVolume blocked when the queue filled up")
	}
}

// Reaching for the mouse means the card has been read and is now in the way.
func TestClickDismissesTheCard(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1, DismissOnClick: true}, testLogger())
	ctx := context.Background()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50}, ctx)
	o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	if o.ph != holding {
		t.Fatalf("setup: want holding, got %v", o.ph)
	}

	// The first poll after the card appears only primes the button state.
	held := func() bool { return true }
	o.checkDismiss(time.Now(), held)
	if o.ph != holding {
		t.Fatal("a button already held when the card appeared must not dismiss it")
	}

	o.checkDismiss(time.Now(), held)
	if o.ph != exiting {
		t.Fatalf("a click should start the fade-out, got %v", o.ph)
	}
}

func TestClickIgnoredWhenDisabled(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1, DismissOnClick: false}, testLogger())
	ctx := context.Background()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50}, ctx)
	o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	o.primeClick = false

	o.checkDismiss(time.Now(), func() bool { return true })
	if o.ph != holding {
		t.Fatalf("dismiss_on_click is off; the card should stay, got %v", o.ph)
	}
}

// A track switched to must show its progress line at the start, not wherever
// that track was left last time it played.
func TestProgressResetsForANewTrack(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	ctx := context.Background()
	now := time.Now()

	// Something already half-played.
	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50, track: Track{
		Title: "Old", Duration: 200 * time.Second, Position: 100 * time.Second,
		PositionAt: now, Playing: true,
	}}, ctx)
	st, _, _ := o.advance(now)
	if st.progress < 0.4 || st.progress > 0.6 {
		t.Fatalf("setup: want about half, got %.2f", st.progress)
	}

	// Then a skip, which always starts at zero.
	o.apply(event{typ: evTrack, kind: KindTrack, track: Track{
		Title: "New", Duration: 180 * time.Second, Position: 0,
		PositionAt: time.Now(), Playing: true,
	}}, ctx)
	st, _, _ = o.advance(time.Now())
	if st.progress > 0.02 {
		t.Fatalf("a new track must start at zero, got %.2f", st.progress)
	}
}

// A card that does not know its track must not keep drawing the previous
// track's progress. This is what made a skip look like it resumed mid-song.
func TestUnknownTrackClearsProgress(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	ctx := context.Background()
	now := time.Now()

	o.apply(event{typ: evVolume, kind: KindVolume, volume: 50, track: Track{
		Title: "Old", Duration: 200 * time.Second, Position: 150 * time.Second,
		PositionAt: now, Playing: true,
	}}, ctx)
	if st, _, _ := o.advance(now); st.progress < 0.7 {
		t.Fatalf("setup: want a well-advanced track, got %.2f", st.progress)
	}

	// A skip whose destination is not known yet: no title, no duration.
	o.apply(event{typ: evTrack, kind: KindTrack, pending: true,
		track: Track{ArtURL: "keep-the-cover"}}, ctx)

	st, _, _ := o.advance(time.Now())
	if st.progress != 0 {
		t.Fatalf("an unknown track must show no progress, got %.2f", st.progress)
	}
}

// The card's own rectangle excludes the transparent padding the canvas keeps
// for its shadow, so a click just outside the visible panel is not a click on
// it.
func TestCardRectCoversOnlyTheVisiblePanel(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	l := o.layout
	box := o.cardRect(1000, 500)

	inX := 1000 + int(l.cardX) + int(l.cardW)/2
	inY := 500 + int(l.cardTop(l.cardH)) + int(l.cardH)/2
	if !box.contains(inX, inY) {
		t.Fatalf("the middle of the card should be inside %v", box)
	}
	// Just left of the panel, still inside the window's padding.
	if box.contains(1000+2, inY) {
		t.Fatal("the transparent margin must not count as the card")
	}
	// Below the card, in the bottom padding.
	if box.contains(inX, 500+int(l.canvasH)-2) {
		t.Fatal("the shadow padding must not count as the card")
	}
}

// The peek card is taller, so its hit area has to grow with it.
func TestCardRectFollowsThePeekHeight(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	normal := o.cardRect(0, 0)
	o.content.kind = KindPeek
	peek := o.cardRect(0, 0)

	if peek.y1-peek.y0 <= normal.y1-normal.y0 {
		t.Fatal("the peek card should have a taller hit area")
	}
	if peek.y1 != normal.y1 {
		t.Fatalf("both cards share a bottom edge: %d vs %d", peek.y1, normal.y1)
	}
}

// A correction that arrives after the card has gone must not bring it back.
// The track is playing by then, so a card announcing it as "next" is wrong.
func TestCorrectionDoesNotReviveAFinishedCard(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	ctx := context.Background()

	o.apply(event{typ: evTrack, kind: KindTrack, dir: Forward,
		track: Track{Title: "Guessed"}}, ctx)
	o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	o.advance(o.holdUntil.Add(time.Millisecond))
	_, _, visible := o.advance(o.phaseStart.Add(exitDur + time.Millisecond))
	if visible || o.ph != hidden {
		t.Fatalf("setup: card should be gone, got %v", o.ph)
	}

	o.apply(event{typ: evTrack, kind: KindTrack, dir: Forward, correct: true,
		track: Track{Title: "Actually Playing"}}, ctx)

	if o.ph != hidden {
		t.Fatalf("a correction must not bring the card back, got %v", o.ph)
	}
}

// While the card is still up, a correction fixes it in place without
// restarting the animation or extending its stay.
func TestCorrectionUpdatesAVisibleCardInPlace(t *testing.T) {
	o := New(Options{Enabled: true, Scale: 1}, testLogger())
	ctx := context.Background()

	o.apply(event{typ: evTrack, kind: KindTrack, dir: Forward, pending: true,
		track: Track{}}, ctx)
	o.advance(o.phaseStart.Add(enterDur + time.Millisecond))
	hold := o.holdUntil

	o.apply(event{typ: evTrack, kind: KindTrack, dir: Forward, correct: true,
		track: Track{Title: "Real Track", Artist: "Real Artist"}}, ctx)

	if o.ph != holding {
		t.Fatalf("the card should still be holding, got %v", o.ph)
	}
	if !o.holdUntil.Equal(hold) {
		t.Fatal("a correction must not extend how long the card stays")
	}
	if o.content.title != "Real Track" || o.content.pending {
		t.Fatalf("the card was not corrected: %+v", o.content)
	}
}
