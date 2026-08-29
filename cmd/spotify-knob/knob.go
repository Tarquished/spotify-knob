package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"spotify-knob/internal/hotkey"
)

// The knob has four gestures on one button and one axis, so the routing has to
// be a real state machine rather than a pile of timers:
//
//	turn                     volume
//	click                    next track
//	double click             previous track
//	Alt + click              previous track, with no double-click wait
//	Alt + turn               open the queue peek and move the highlight
//	Ctrl + click             open or close the lyrics panel
//	click while peeking      play the highlighted track
//
// The queue peek is deliberately not a press-and-hold. On this keyboard a
// long hold of the knob is claimed by the firmware for its own lighting menu,
// which never reaches Windows and so cannot be intercepted from here; the
// hold gesture is still available behind peek_gesture: "hold" for hardware
// that leaves it alone.
//
// A click cannot be dispatched until the double-click window closes, unless
// that window is set to zero, in which case a click fires immediately and
// previous is reached with Alt. Everything runs on one goroutine and timers
// report back through a channel, so there is no shared state to race over.

type knobDeck interface {
	Adjust(ctx context.Context, delta int)
	Next(ctx context.Context)
	Previous(ctx context.Context)
	PlayQueued(ctx context.Context, index int)
}

type peekUI interface {
	ShowPeek(selected int, hold time.Duration)
}

// lyricsToggler opens and closes the lyrics panel. Toggle may block on the
// network, so the router always calls it on its own goroutine.
type lyricsToggler interface {
	Toggle(ctx context.Context)
}

// gestureConfig is swapped atomically so a config reload takes effect on the
// next gesture without restarting anything.
type peekGesture int

const (
	peekAltTurn peekGesture = iota // Alt + turn
	peekHold                       // press and hold
	peekOff
)

func parsePeekGesture(s string) peekGesture {
	switch s {
	case "hold":
		return peekHold
	case "off", "none", "disabled":
		return peekOff
	default:
		return peekAltTurn
	}
}

type gestureConfig struct {
	doublePress time.Duration
	longPress   time.Duration
	peekLinger  time.Duration
	peekBrowse  time.Duration
	peek        peekGesture
}

// heldPeekHold is how long the peek card is told to survive while the knob is
// still down. It doubles as the safety net for a release event that never
// arrives.
const heldPeekHold = 20 * time.Second

type knobState int

const (
	stIdle       knobState = iota
	stPressed              // down, still deciding between click and hold
	stPeek                 // hold engaged, knob still down
	stPeekLinger           // released, the browse window is still open
	stConsumed             // the press did something; swallow its release
)

type timerKind int

const (
	tHold timerKind = iota
	tClick
	tLinger
	tSafety
)

type timerFire struct {
	kind timerKind
	gen  uint64
}

type knobRouter struct {
	deck     knobDeck
	ui       peekUI
	cfg      *atomic.Pointer[gestureConfig]
	queueLen *atomic.Int64
	altDown  func() bool
	ctrlDown func() bool
	lyrics   lyricsToggler
	log      *slog.Logger

	state    knobState
	selected int

	gen          map[timerKind]uint64
	clickPending bool
	timers       chan timerFire
}

func newKnobRouter(deck knobDeck, ui peekUI, cfg *atomic.Pointer[gestureConfig],
	queueLen *atomic.Int64, altDown, ctrlDown func() bool, lyr lyricsToggler,
	log *slog.Logger) *knobRouter {
	if altDown == nil {
		altDown = func() bool { return false }
	}
	if ctrlDown == nil {
		ctrlDown = func() bool { return false }
	}
	return &knobRouter{
		deck:     deck,
		ui:       ui,
		cfg:      cfg,
		queueLen: queueLen,
		altDown:  altDown,
		ctrlDown: ctrlDown,
		lyrics:   lyr,
		log:      log,
		gen:      make(map[timerKind]uint64),
		timers:   make(chan timerFire, 16),
	}
}

// run consumes key events until ctx is cancelled or the hook stops.
func (r *knobRouter) run(ctx context.Context, events <-chan hotkey.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			r.onKey(ctx, ev)
		case f := <-r.timers:
			r.onTimer(ctx, f)
		}
	}
}

// schedule arms a timer, invalidating any previous one of the same kind.
func (r *knobRouter) schedule(kind timerKind, d time.Duration) {
	r.gen[kind]++
	gen := r.gen[kind]
	time.AfterFunc(d, func() {
		select {
		case r.timers <- timerFire{kind: kind, gen: gen}:
		default:
		}
	})
}

// cancel invalidates a pending timer without stopping it; a late fire is
// recognised by its stale generation and ignored.
func (r *knobRouter) cancel(kind timerKind) { r.gen[kind]++ }

func (r *knobRouter) onKey(ctx context.Context, ev hotkey.Event) {
	cfg := r.cfg.Load()

	switch ev {
	case hotkey.VolumeUp:
		r.onTurn(ctx, +1, +1, cfg)
	case hotkey.VolumeDown:
		r.onTurn(ctx, -1, -1, cfg)
	case hotkey.Press:
		r.onPress(ctx, cfg)
	case hotkey.PressUp:
		r.onRelease(ctx, cfg)
	}
}

// onTurn routes a rotation: it moves the highlight while the peek is open and
// changes the volume otherwise. selDelta and volDelta point the same way, so
// the knob's clockwise direction always means "further along": louder when
// setting the volume, further down the queue when browsing it. The queue is
// drawn top to bottom in play order, so down the screen is forward in time,
// and matching the knob to the list beats matching it to the screen axis.
func (r *knobRouter) onTurn(ctx context.Context, selDelta, volDelta int, cfg *gestureConfig) {
	browsing := r.state == stPeek || r.state == stPeekLinger

	if !browsing {
		if cfg.peek == peekAltTurn && r.altDown() {
			// First Alt-turn opens the list rather than moving inside it, so
			// the gesture always starts from a known place.
			r.state = stPeekLinger
			r.selected = 0
			r.schedule(tLinger, cfg.peekBrowse)
			r.ui.ShowPeek(0, cfg.peekBrowse)
			r.log.Debug("knob: peek opened", "via", "alt-turn")
			return
		}
		r.deck.Adjust(ctx, volDelta)
		return
	}

	n := int(r.queueLen.Load())
	if n == 0 {
		return
	}
	r.selected = clampInt(r.selected+selDelta, 0, n-1)

	hold := heldPeekHold
	if r.state == stPeekLinger {
		// Browsing after the release keeps the window open, otherwise the
		// card would vanish mid-decision.
		hold = cfg.peekBrowse
		r.schedule(tLinger, hold)
	}
	r.ui.ShowPeek(r.selected, hold)
}

func (r *knobRouter) onPress(ctx context.Context, cfg *gestureConfig) {
	switch r.state {
	case stPeek, stPeekLinger:
		// A press while browsing commits to the highlighted track.
		index := r.selected
		r.cancel(tLinger)
		r.cancel(tSafety)
		r.state = stConsumed
		go r.deck.PlayQueued(context.WithoutCancel(ctx), index)
		r.log.Debug("knob: play queued", "index", index)

	case stIdle:
		if r.lyrics != nil && r.ctrlDown() {
			// Ctrl names a different job entirely, so it never becomes a skip.
			r.clickPending = false
			r.cancel(tClick)
			r.state = stConsumed
			go r.lyrics.Toggle(context.WithoutCancel(ctx))
			r.log.Debug("knob: lyrics toggled")
			return
		}
		if r.altDown() {
			// Alt names the direction outright, so there is nothing to wait
			// for: previous fires on the spot.
			r.clickPending = false
			r.cancel(tClick)
			r.state = stConsumed
			go r.deck.Previous(context.WithoutCancel(ctx))
			return
		}
		if r.clickPending {
			// Second click inside the window: previous track.
			r.clickPending = false
			r.cancel(tClick)
			r.state = stConsumed
			go r.deck.Previous(context.WithoutCancel(ctx))
			return
		}
		r.state = stPressed
		if cfg.peek == peekHold {
			r.schedule(tHold, cfg.longPress)
		}
	}
}

func (r *knobRouter) onRelease(ctx context.Context, cfg *gestureConfig) {
	switch r.state {
	case stPressed:
		r.cancel(tHold)
		r.state = stIdle
		if cfg.doublePress <= 0 {
			// Double-click detection turned off: nothing to wait for, so the
			// skip goes out immediately and previous lives on Alt+click.
			go r.deck.Next(context.WithoutCancel(ctx))
			return
		}
		// Otherwise a click still has to outlast the double-click window
		// before it can be called a single.
		r.clickPending = true
		r.schedule(tClick, cfg.doublePress)

	case stPeek:
		r.state = stPeekLinger
		r.schedule(tLinger, cfg.peekLinger)
		r.cancel(tSafety)
		r.ui.ShowPeek(r.selected, cfg.peekLinger)

	case stConsumed:
		r.state = stIdle
	}
}

func (r *knobRouter) onTimer(ctx context.Context, f timerFire) {
	if f.gen != r.gen[f.kind] {
		return // superseded
	}
	switch f.kind {
	case tHold:
		if r.state != stPressed {
			return
		}
		r.state = stPeek
		r.selected = 0
		r.schedule(tSafety, heldPeekHold)
		r.ui.ShowPeek(0, heldPeekHold)
		r.log.Debug("knob: peek opened")

	case tClick:
		if !r.clickPending {
			return
		}
		r.clickPending = false
		go r.deck.Next(context.WithoutCancel(ctx))

	case tLinger:
		if r.state == stPeekLinger {
			r.state = stIdle
		}

	case tSafety:
		// A release we never saw. Do not leave the knob stuck in peek mode.
		if r.state == stPeek {
			r.log.Debug("knob: peek released by safety timer")
			r.state = stIdle
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
