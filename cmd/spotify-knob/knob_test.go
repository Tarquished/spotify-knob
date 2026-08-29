package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spotify-knob/internal/hotkey"
)

type fakeDeck struct {
	mu     sync.Mutex
	delta  int
	next   int
	prev   int
	played []int
}

func (d *fakeDeck) Adjust(_ context.Context, v int) {
	d.mu.Lock()
	d.delta += v
	d.mu.Unlock()
}
func (d *fakeDeck) Next(context.Context) {
	d.mu.Lock()
	d.next++
	d.mu.Unlock()
}
func (d *fakeDeck) Previous(context.Context) {
	d.mu.Lock()
	d.prev++
	d.mu.Unlock()
}
func (d *fakeDeck) PlayQueued(_ context.Context, i int) {
	d.mu.Lock()
	d.played = append(d.played, i)
	d.mu.Unlock()
}
func (d *fakeDeck) snap() (delta, next, prev int, played []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delta, d.next, d.prev, append([]int(nil), d.played...)
}

type fakePeek struct {
	mu    sync.Mutex
	calls []int
}

func (p *fakePeek) ShowPeek(sel int, _ time.Duration) {
	p.mu.Lock()
	p.calls = append(p.calls, sel)
	p.mu.Unlock()
}
func (p *fakePeek) seen() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.calls...)
}

// Deliberately short so the tests stay fast; the ratios match the defaults.
var testGestures = gestureConfig{
	doublePress: 80 * time.Millisecond,
	longPress:   120 * time.Millisecond,
	peekLinger:  200 * time.Millisecond,
	peekBrowse:  300 * time.Millisecond,
	peek:        peekAltTurn,
}

// alt is a switchable stand-in for the real modifier check.
type alt struct{ down atomic.Bool }

func (a *alt) pressed() bool { return a.down.Load() }

// fakeLyrics counts the toggles the router asks for.
type fakeLyrics struct{ calls atomic.Int64 }

func (f *fakeLyrics) Toggle(context.Context) { f.calls.Add(1) }

func startRouterWith(t *testing.T, queueLen int, c gestureConfig) (chan hotkey.Event, *fakeDeck, *fakePeek, *alt) {
	events, deck, ui, a, _ := startRouterMod(t, queueLen, c)
	return events, deck, ui, a
}

// startRouterMod also hands back the Ctrl modifier and the lyrics stand-in,
// for the tests that care about them.
func startRouterMod(t *testing.T, queueLen int, c gestureConfig) (chan hotkey.Event, *fakeDeck, *fakePeek, *alt, *ctrlLyrics) {
	t.Helper()
	deck := &fakeDeck{}
	ui := &fakePeek{}
	cfg := &atomic.Pointer[gestureConfig]{}
	cfg.Store(&c)
	n := &atomic.Int64{}
	n.Store(int64(queueLen))
	a := &alt{}

	events := make(chan hotkey.Event, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cl := &ctrlLyrics{}
	r := newKnobRouter(deck, ui, cfg, n, a.pressed, cl.ctrl.pressed, &cl.lyr,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	go r.run(ctx, events)
	return events, deck, ui, a, cl
}

// ctrlLyrics bundles the Ctrl modifier with the panel it opens.
type ctrlLyrics struct {
	ctrl alt
	lyr  fakeLyrics
}

func startRouter(t *testing.T, queueLen int) (chan hotkey.Event, *fakeDeck, *fakePeek) {
	t.Helper()
	events, deck, ui, _ := startRouterWith(t, queueLen, testGestures)
	return events, deck, ui
}

func click(events chan hotkey.Event) {
	events <- hotkey.Press
	events <- hotkey.PressUp
}

func TestClickIsNext(t *testing.T) {
	events, deck, _ := startRouter(t, 4)
	click(events)
	time.Sleep(300 * time.Millisecond)

	_, next, prev, _ := deck.snap()
	if next != 1 || prev != 0 {
		t.Fatalf("want next=1 prev=0, got next=%d prev=%d", next, prev)
	}
}

// The regression this guards: a double click leaking an extra next after the
// previous has already fired.
func TestDoubleClickIsPreviousOnly(t *testing.T) {
	events, deck, _ := startRouter(t, 4)
	click(events)
	time.Sleep(30 * time.Millisecond)
	click(events)
	time.Sleep(400 * time.Millisecond)

	_, next, prev, _ := deck.snap()
	if prev != 1 {
		t.Fatalf("want prev=1, got %d", prev)
	}
	if next != 0 {
		t.Fatalf("double click leaked %d next command(s)", next)
	}
}

func TestSpacedClicksAreTwoNexts(t *testing.T) {
	events, deck, _ := startRouter(t, 4)
	click(events)
	time.Sleep(200 * time.Millisecond)
	click(events)
	time.Sleep(300 * time.Millisecond)

	_, next, _, _ := deck.snap()
	if next != 2 {
		t.Fatalf("want next=2, got %d", next)
	}
}

func TestTurnChangesVolume(t *testing.T) {
	events, deck, _ := startRouter(t, 4)
	for i := 0; i < 3; i++ {
		events <- hotkey.VolumeUp
	}
	events <- hotkey.VolumeDown
	time.Sleep(150 * time.Millisecond)

	delta, _, _, _ := deck.snap()
	if delta != 2 {
		t.Fatalf("want net delta 2, got %d", delta)
	}
}

// Holding opens the peek, and must not also fire a track skip.
func TestHoldOpensPeekWithoutSkipping(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, deck, ui, _ := startRouterWith(t, 4, cfg)
	events <- hotkey.Press
	time.Sleep(250 * time.Millisecond)
	events <- hotkey.PressUp
	time.Sleep(100 * time.Millisecond)

	if len(ui.seen()) == 0 {
		t.Fatal("hold did not open the peek")
	}
	_, next, prev, _ := deck.snap()
	if next != 0 || prev != 0 {
		t.Fatalf("hold should not skip: next=%d prev=%d", next, prev)
	}
}

// While the peek is open the knob browses instead of changing volume.
func TestTurnWhilePeekingMovesSelection(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, deck, ui, _ := startRouterWith(t, 4, cfg)
	events <- hotkey.Press
	time.Sleep(200 * time.Millisecond) // hold engages

	events <- hotkey.VolumeUp // move down the list
	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)

	seen := ui.seen()
	if len(seen) == 0 || seen[len(seen)-1] != 2 {
		t.Fatalf("want selection 2, got %v", seen)
	}
	if delta, _, _, _ := deck.snap(); delta != 0 {
		t.Fatalf("volume must not move while peeking, got delta %d", delta)
	}
}

func TestSelectionClampsToQueue(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, _, ui, _ := startRouterWith(t, 2, cfg)
	events <- hotkey.Press
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 6; i++ {
		events <- hotkey.VolumeUp
	}
	time.Sleep(150 * time.Millisecond)

	seen := ui.seen()
	if got := seen[len(seen)-1]; got != 1 {
		t.Fatalf("selection should stop at the last row (1), got %d", got)
	}
}

// The whole point of the gesture: browse, then press to play what is chosen.
func TestPressWhilePeekingPlaysSelection(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, deck, _, _ := startRouterWith(t, 4, cfg)
	events <- hotkey.Press
	time.Sleep(200 * time.Millisecond)
	events <- hotkey.VolumeUp
	events <- hotkey.PressUp // released, linger window open
	time.Sleep(50 * time.Millisecond)

	click(events) // choose
	time.Sleep(200 * time.Millisecond)

	_, next, prev, played := deck.snap()
	if len(played) != 1 || played[0] != 1 {
		t.Fatalf("want one play of index 1, got %v", played)
	}
	if next != 0 || prev != 0 {
		t.Fatalf("choosing must not also skip: next=%d prev=%d", next, prev)
	}
}

// Once the browse window lapses the knob goes back to being a volume knob.
func TestVolumeResumesAfterPeekExpires(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, deck, _, _ := startRouterWith(t, 4, cfg)
	events <- hotkey.Press
	time.Sleep(200 * time.Millisecond)
	events <- hotkey.PressUp
	time.Sleep(400 * time.Millisecond) // past peekLinger

	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)

	if delta, _, _, _ := deck.snap(); delta != 1 {
		t.Fatalf("volume should work again after the peek closed, got delta %d", delta)
	}
}

// Turning during the linger has to extend it, or the card vanishes while the
// user is still deciding.
func TestBrowsingExtendsTheLinger(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekHold
	events, deck, _, _ := startRouterWith(t, 4, cfg)
	events <- hotkey.Press
	time.Sleep(200 * time.Millisecond)
	events <- hotkey.PressUp

	// Keep nudging past the base linger; the peek must stay open.
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		events <- hotkey.VolumeDown
	}
	time.Sleep(50 * time.Millisecond)

	if delta, _, _, _ := deck.snap(); delta != 0 {
		t.Fatalf("still browsing, volume must not move: delta %d", delta)
	}
}

// The knob's own long hold is claimed by the keyboard firmware, so the peek
// has to be reachable without holding anything.
func TestAltTurnOpensPeek(t *testing.T) {
	events, deck, ui, a := startRouterWith(t, 4, testGestures)
	a.down.Store(true)

	events <- hotkey.VolumeDown
	time.Sleep(120 * time.Millisecond)

	seen := ui.seen()
	if len(seen) == 0 {
		t.Fatal("Alt+turn did not open the peek")
	}
	if seen[0] != 0 {
		t.Fatalf("the opening turn should land on the first row, got %d", seen[0])
	}
	if delta, _, _, _ := deck.snap(); delta != 0 {
		t.Fatalf("Alt+turn must not touch the volume, got delta %d", delta)
	}

	// Subsequent turns browse.
	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)
	seen = ui.seen()
	if seen[len(seen)-1] != 1 {
		t.Fatalf("want selection 1, got %v", seen)
	}
}

// Ctrl claims the press for the lyrics panel, so it must not also skip.
func TestCtrlClickOpensLyrics(t *testing.T) {
	events, deck, _, _, cl := startRouterMod(t, 4, testGestures)
	cl.ctrl.down.Store(true)

	click(events)
	time.Sleep(200 * time.Millisecond)

	if got := cl.lyr.calls.Load(); got != 1 {
		t.Fatalf("want one lyrics toggle, got %d", got)
	}
	if _, next, prev, _ := deck.snap(); next != 0 || prev != 0 {
		t.Fatalf("Ctrl+click must not skip: next=%d prev=%d", next, prev)
	}
}

// Releasing Ctrl puts the press back to being a skip.
func TestClickWithoutCtrlDoesNotOpenLyrics(t *testing.T) {
	events, deck, _, _, cl := startRouterMod(t, 4, testGestures)

	click(events)
	time.Sleep(200 * time.Millisecond)

	if got := cl.lyr.calls.Load(); got != 0 {
		t.Fatalf("want no lyrics toggle, got %d", got)
	}
	if _, next, _, _ := deck.snap(); next != 1 {
		t.Fatalf("want one next, got %d", next)
	}
}

// Ctrl is a press modifier only: turning the knob is still the volume.
func TestCtrlTurnIsStillVolume(t *testing.T) {
	events, deck, _, _, cl := startRouterMod(t, 4, testGestures)
	cl.ctrl.down.Store(true)

	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)

	if delta, _, _, _ := deck.snap(); delta != 1 {
		t.Fatalf("want delta 1, got %d", delta)
	}
	if got := cl.lyr.calls.Load(); got != 0 {
		t.Fatalf("a turn is not a press: %d toggles", got)
	}
}

// Without the modifier the knob is still just a volume knob.
func TestTurnWithoutAltIsStillVolume(t *testing.T) {
	events, deck, ui, _ := startRouterWith(t, 4, testGestures)
	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)

	if len(ui.seen()) != 0 {
		t.Fatal("peek should not open without the modifier")
	}
	if delta, _, _, _ := deck.snap(); delta != 1 {
		t.Fatalf("want delta 1, got %d", delta)
	}
}

// Alt names the direction, so previous needs no double-click wait.
func TestAltClickIsPreviousImmediately(t *testing.T) {
	events, deck, _, a := startRouterWith(t, 4, testGestures)
	a.down.Store(true)

	start := time.Now()
	click(events)
	for time.Since(start) < time.Second {
		if _, _, prev, _ := deck.snap(); prev == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)

	_, next, prev, _ := deck.snap()
	if prev != 1 || next != 0 {
		t.Fatalf("want prev=1 next=0, got prev=%d next=%d", prev, next)
	}
	if elapsed >= testGestures.doublePress {
		t.Fatalf("Alt+click waited %s; it should not wait out the double-click window", elapsed)
	}
}

// With the double-click window disabled a click skips on the spot.
func TestZeroDoublePressSkipsImmediately(t *testing.T) {
	cfg := testGestures
	cfg.doublePress = 0
	events, deck, _, _ := startRouterWith(t, 4, cfg)

	start := time.Now()
	click(events)
	for time.Since(start) < time.Second {
		if _, next, _, _ := deck.snap(); next == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Millisecond {
		t.Fatalf("click took %s with the wait disabled", elapsed)
	}
	if _, next, prev, _ := deck.snap(); next != 1 || prev != 0 {
		t.Fatalf("want next=1 prev=0, got next=%d prev=%d", next, prev)
	}
}

// Peek turned off leaves the knob doing nothing but volume.
func TestPeekOffKeepsVolumeOnAltTurn(t *testing.T) {
	cfg := testGestures
	cfg.peek = peekOff
	events, deck, ui, a := startRouterWith(t, 4, cfg)
	a.down.Store(true)

	events <- hotkey.VolumeUp
	time.Sleep(120 * time.Millisecond)

	if len(ui.seen()) != 0 {
		t.Fatal("peek is disabled and must not open")
	}
	if delta, _, _, _ := deck.snap(); delta != 1 {
		t.Fatalf("want delta 1, got %d", delta)
	}
}
