package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"spotify-knob/internal/controller"
	"spotify-knob/internal/lyrics"
	"spotify-knob/internal/osd"
)

// fakePanel records what the manager asked the window to do.
type fakePanel struct {
	mu      sync.Mutex
	visible bool
	shown   int
	hidden  int
	states  []string
	docs    []*osd.LyricDoc
}

func (p *fakePanel) Visible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.visible
}

func (p *fakePanel) Show() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.visible, p.shown = true, p.shown+1
}

func (p *fakePanel) Hide() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.visible, p.hidden = false, p.hidden+1
}

func (p *fakePanel) SetTrack(osd.LyricsTrack) {}

func (p *fakePanel) note(s string, doc *osd.LyricDoc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = append(p.states, s)
	p.docs = append(p.docs, doc)
}

func (p *fakePanel) Loading(string)                  { p.note("loading", nil) }
func (p *fakePanel) Ready(_ string, d *osd.LyricDoc) { p.note("ready", d) }
func (p *fakePanel) Missing(string)                  { p.note("missing", nil) }
func (p *fakePanel) Failed(string)                   { p.note("failed", nil) }

func (p *fakePanel) snap() (shown, hidden int, states []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shown, p.hidden, append([]string(nil), p.states...)
}

// fakeCard records the small overlay notices.
type fakeCard struct {
	mu       sync.Mutex
	messages []string
}

func (c *fakeCard) ShowNotice(_, message string, _ osd.Track) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, message)
}

func (c *fakeCard) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.messages...)
}

// fakeSource stands in for LRCLIB.
type fakeSource struct {
	doc    *lyrics.Lyrics
	err    error
	cached bool          // answer from Peek without a lookup
	delay  time.Duration // how long Get takes
	calls  int
	mu     sync.Mutex
}

func (s *fakeSource) Get(ctx context.Context, _ lyrics.Query) (*lyrics.Lyrics, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.doc, s.err
}

func (s *fakeSource) Peek(lyrics.Query) (*lyrics.Lyrics, bool) {
	if !s.cached {
		return nil, false
	}
	return s.doc, true
}

func testManager(src lyricsSource) (*lyricsManager, *fakePanel, *fakeCard) {
	panel := &fakePanel{}
	card := &fakeCard{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &lyricsManager{panel: panel, card: card, prov: src, log: log}, panel, card
}

func playing(title string) controller.NowPlaying {
	return controller.NowPlaying{
		Title: title, Artist: "An Artist", Album: "An Album",
		URI: "spotify:track:" + title, Duration: 3 * time.Minute,
	}
}

var sampleDoc = &lyrics.Lyrics{
	Synced: true, Source: "LRCLIB",
	Lines: []lyrics.Line{{At: time.Second, Text: "hello"}},
}

// The point of the whole open-or-decline dance: a track with no lyrics never
// opens an empty panel, it just says so on the small card.
func TestToggleDeclinesWhenThereAreNoLyrics(t *testing.T) {
	src := &fakeSource{err: lyrics.ErrNotFound}
	m, panel, card := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	shown, _, _ := panel.snap()
	if shown != 0 {
		t.Fatalf("the panel must not open with nothing to show (shown %d)", shown)
	}
	if got := card.seen(); len(got) != 1 || got[0] != "No lyrics for this track" {
		t.Fatalf("want the no-lyrics notice, got %v", got)
	}
}

func TestToggleOpensWithLyrics(t *testing.T) {
	src := &fakeSource{doc: sampleDoc}
	m, panel, card := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	shown, _, states := panel.snap()
	if shown != 1 {
		t.Fatalf("want the panel opened once, got %d", shown)
	}
	if len(states) == 0 || states[len(states)-1] != "ready" {
		t.Fatalf("want the panel to end up ready, got %v", states)
	}
	if len(card.seen()) != 0 {
		t.Fatalf("no notice should be shown when the panel opens: %v", card.seen())
	}
}

// A cached answer must not go near the network.
func TestToggleUsesTheCacheWithoutLookingUp(t *testing.T) {
	src := &fakeSource{doc: sampleDoc, cached: true}
	m, panel, _ := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	if src.calls != 0 {
		t.Fatalf("a cached hit should not call Get, got %d calls", src.calls)
	}
	if shown, _, _ := panel.snap(); shown != 1 {
		t.Fatalf("want the panel opened, got %d", shown)
	}
}

func TestToggleUsesACachedMiss(t *testing.T) {
	src := &fakeSource{cached: true} // cached, and the answer is "none"
	m, panel, card := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	if src.calls != 0 {
		t.Fatalf("a cached miss should not call Get, got %d calls", src.calls)
	}
	if shown, _, _ := panel.snap(); shown != 0 {
		t.Fatal("a cached miss should not open the panel")
	}
	if len(card.seen()) != 1 {
		t.Fatalf("want one notice, got %v", card.seen())
	}
}

// A slow lookup opens the panel in its loading state rather than leaving the
// press unanswered, and fills it in when the words land.
func TestSlowLookupOpensLoadingThenFillsIn(t *testing.T) {
	src := &fakeSource{doc: sampleDoc, delay: openWait + 200*time.Millisecond}
	m, panel, _ := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	shown, _, states := panel.snap()
	if shown != 1 {
		t.Fatalf("want the panel opened while loading, got %d", shown)
	}
	if len(states) == 0 || states[0] != "loading" {
		t.Fatalf("want a loading state first, got %v", states)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, s := panel.snap(); s[len(s)-1] == "ready" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, s := panel.snap()
	t.Fatalf("the words never arrived: %v", s)
}

// Once the panel is open a track without lyrics says so in place. Closing the
// window the user deliberately opened would be worse than an honest message.
func TestSlowMissLeavesTheOpenPanelUp(t *testing.T) {
	src := &fakeSource{err: lyrics.ErrNotFound, delay: openWait + 150*time.Millisecond}
	m, panel, card := testManager(src)
	m.toggleFor(context.Background(), playing("Some Song"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, s := panel.snap(); s[len(s)-1] == "missing" {
			if _, hidden, _ := panel.snap(); hidden != 0 {
				t.Fatal("an open panel should not be closed by a miss")
			}
			if len(card.seen()) != 0 {
				t.Fatalf("no card once the panel is up, got %v", card.seen())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the miss was never reported to the panel")
}

// The regression this exists for: a slow lookup landing after the user has
// already closed the panel used to re-open it, which is the window equivalent
// of the overlay card reviving after its fade-out.
func TestLateLookupDoesNotReopenAClosedPanel(t *testing.T) {
	src := &fakeSource{doc: sampleDoc, delay: openWait + 400*time.Millisecond}
	m, panel, _ := testManager(src)

	go m.toggleFor(context.Background(), playing("Some Song"))

	// Wait for the loading panel to come up, then close it the way a second
	// knob press would.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !panel.Visible() {
		time.Sleep(10 * time.Millisecond)
	}
	if !panel.Visible() {
		t.Fatal("the panel never opened in its loading state")
	}
	panel.Hide()

	time.Sleep(openWait + 600*time.Millisecond) // let the lookup land
	if panel.Visible() {
		t.Fatal("a lookup that lands after the user closed the panel must not reopen it")
	}
}

// The same rule for the follower: a track change under a closed panel is not
// a reason to put it back up.
func TestFollowerDoesNotReopenAClosedPanel(t *testing.T) {
	src := &fakeSource{doc: sampleDoc}
	m, panel, _ := testManager(src)

	m.deliver(playing("Some Song"), sampleDoc, nil, false)
	if panel.Visible() {
		t.Fatal("a non-deciding delivery must never open the panel")
	}
	if _, _, states := panel.snap(); len(states) != 0 {
		t.Fatalf("a closed panel should not be written to at all, got %v", states)
	}
}

// Two presses inside the lookup window mean open then changed-my-mind, not
// two opens racing each other.
func TestSecondPressCancelsAPendingOpen(t *testing.T) {
	src := &fakeSource{doc: sampleDoc, delay: 250 * time.Millisecond}
	m, panel, _ := testManager(src)

	go m.toggleFor(context.Background(), playing("Some Song"))
	time.Sleep(60 * time.Millisecond)
	m.toggleFor(context.Background(), playing("Some Song")) // the cancelling press

	time.Sleep(700 * time.Millisecond)
	if shown, _, _ := panel.snap(); shown != 0 {
		t.Fatalf("the cancelled open should not have opened the panel (shown %d)", shown)
	}
}

func TestTogglePressClosesAnOpenPanel(t *testing.T) {
	src := &fakeSource{doc: sampleDoc, cached: true}
	m, panel, _ := testManager(src)
	panel.Show()

	m.Toggle(context.Background())
	if _, hidden, _ := panel.snap(); hidden != 1 {
		t.Fatal("a press with the panel open should close it")
	}
	if src.calls != 0 {
		t.Fatal("closing must not look anything up")
	}
}

func TestToggleWithNothingPlaying(t *testing.T) {
	src := &fakeSource{doc: sampleDoc}
	m, panel, card := testManager(src)
	m.toggleFor(context.Background(), playing(""))

	if shown, _, _ := panel.snap(); shown != 0 {
		t.Fatal("nothing playing should not open the panel")
	}
	if got := card.seen(); len(got) != 1 || got[0] != "Nothing is playing" {
		t.Fatalf("want the nothing-playing notice, got %v", got)
	}
}
