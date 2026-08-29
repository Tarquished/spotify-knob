// Package osd draws the on-screen card that appears when the knob changes the
// volume or skips a track.
//
// It is a layered, click-through, always-on-top window whose contents are
// composed in Go and pushed to Windows with UpdateLayeredWindow. That gives
// real per-pixel alpha: rounded corners, a soft shadow and a translucent
// panel, with no opaque rectangle behind them.
//
// The frame loop separates *what* the card says from *where* it is: the card
// is composed at rest and cached, while the slide and fade are applied when
// the pixels are handed to Windows. A frame where only the animation moved
// costs one pass over the buffer instead of a full recompose, which is what
// makes a high refresh rate affordable.
package osd

import (
	"context"
	"fmt"
	"image/color"
	"log/slog"
	"math"
	"runtime"
	"time"
)

// Track is what the card displays about the music.
//
// Position is a reading taken at PositionAt rather than a live value: the
// card extrapolates between polls instead of asking Spotify where the
// playhead is on every frame.
type Track struct {
	Title      string
	Artist     string
	ArtURL     string
	URI        string
	Duration   time.Duration
	Position   time.Duration
	PositionAt time.Time
	Playing    bool
}

func (t Track) empty() bool { return t.Title == "" && t.Artist == "" }

// Options configure the card. They come from config.json.
type Options struct {
	Enabled        bool
	Scale          float64
	Position       string
	HideFullscreen bool
	DismissOnClick bool
	VolumeHold     time.Duration
	TrackHold      time.Duration
	FPS            int // 0 = follow the monitor's refresh rate
}

func (o Options) withDefaults() Options {
	if o.Scale <= 0 {
		o.Scale = 1
	}
	if o.Position == "" {
		o.Position = "bottom-center"
	}
	if o.VolumeHold <= 0 {
		o.VolumeHold = 1500 * time.Millisecond
	}
	if o.TrackHold <= 0 {
		o.TrackHold = 3 * time.Second
	}
	return o
}

const (
	enterDur = 260 * time.Millisecond
	exitDur  = 220 * time.Millisecond
	artFade  = 280 * time.Millisecond
	idleGap  = 50 * time.Millisecond

	// barTau is the time constant of the volume bar's glide. Framerate
	// independent: the per-frame step is derived from the elapsed time, so
	// the bar takes the same wall-clock time to settle at 60 or 240 Hz.
	barTau = 55 * time.Millisecond

	minFPS = 30
	maxFPS = 360

	// refreshEvery re-sends the frame even when nothing changed, so a window
	// switch cannot leave a stale or blank overlay sitting on screen.
	refreshEvery = 250 * time.Millisecond
)

type phase int

const (
	hidden phase = iota
	entering
	holding
	exiting
)

type evType int

const (
	evVolume evType = iota
	evTrack
	evArt
	evPeek
	evQueue
	evConfig
	evNotice
)

type event struct {
	typ      evType
	kind     Kind
	dir      Direction
	volume   int
	track    Track
	pending  bool
	url      string
	art      *artwork
	selected int
	correct  bool
	label    string
	hold     time.Duration
	queue    []Track
	opts     Options
}

// transform is the part of a frame that changes without redrawing the card.
type transform struct {
	offsetY float64
	opacity float64
}

// OSD owns the overlay window and its animation state.
type OSD struct {
	log    *slog.Logger
	opts   Options
	fonts  *fontSet
	art    *artCache
	layout layout
	paint  *painter

	events chan event

	// Animation state, only touched on the window thread.
	ph           phase
	phaseStart   time.Time
	holdUntil    time.Time
	content      frameState
	barCurrent   float64
	lastAdvance  time.Time
	artFadeStart time.Time
	wantURL      string
	lastAccent   color.RGBA

	// Current track timing, used to extrapolate the progress line.
	curDuration   time.Duration
	curPosition   time.Duration
	curPositionAt time.Time
	curPlaying    bool

	// Queue peek.
	queue       []Track
	queueArtBuf []*artwork
	queueRev    int
	selected    int

	frameDur    time.Duration
	needRebuild bool

	shadow  maskCache
	barGlow maskCache

	// Frame cache: what is currently composed in the paint buffer, and what
	// was last handed to Windows.
	haveFrame      bool
	frameKey       contentKey
	shown          bool
	shownTr        transform
	shownPos       [2]int
	lastPresent    time.Time
	lastForeground uintptr

	lastFSCheck time.Time
	primeClick  bool
}

// New builds the OSD. The window itself is created in Run, on its own thread.
func New(opts Options, log *slog.Logger) *OSD {
	opts = opts.withDefaults()
	l := newLayout(opts.Scale*displayScale(), opts.Position)

	return &OSD{
		log:        log,
		opts:       opts,
		fonts:      newFontSet(),
		art:        newArtCache(int(l.artSize), int(l.peekThumb)),
		layout:     l,
		paint:      newPainter(int(l.canvasW), int(l.canvasH)),
		events:     make(chan event, 64),
		lastAccent: colAccentFallback,
	}
}

// ShowPeek displays the queue browser with one row highlighted. It is kept
// alive by repeated calls while the knob is held; hold is how long the card
// should survive the last one.
func (o *OSD) ShowPeek(selected int, hold time.Duration) {
	o.push(event{typ: evPeek, kind: KindPeek, selected: selected, hold: hold})
}

// SetQueue publishes the upcoming tracks and warms their covers, so opening
// the peek does not start with four grey squares.
func (o *OSD) SetQueue(q []Track) {
	o.push(event{typ: evQueue, queue: append([]Track(nil), q...)})
}

// Reconfigure applies a changed config without restarting the daemon.
func (o *OSD) Reconfigure(opts Options) {
	o.push(event{typ: evConfig, opts: opts.withDefaults()})
}

// QueueLen is how many tracks the peek can move through.
func (o *OSD) QueueLen() int {
	// Read without the event queue: callers use this to clamp a selection
	// before sending it, and a stale value by one frame is harmless.
	return len(o.queue)
}

// ShowNotice puts a short message on the card, in the same shape a skip uses.
// It is how the daemon says something small - "this track has no lyrics" -
// without opening a window for it.
func (o *OSD) ShowNotice(label, message string, t Track) {
	o.push(event{typ: evNotice, kind: KindNotice, label: label, track: t, url: message})
}

// ShowVolume displays the volume card. Safe to call from any goroutine, and
// it never blocks: if the queue is full the event is dropped, because a
// dropped frame of a burst is invisible anyway.
func (o *OSD) ShowVolume(volume int, t Track) {
	o.push(event{typ: evVolume, kind: KindVolume, volume: volume, track: t})
}

// ShowTrack displays the track card. Pass pending when the new track is not
// known yet; the card shows the direction immediately and fills in when
// ShowTrack is called again with the details.
func (o *OSD) ShowTrack(dir Direction, t Track, pending bool) {
	o.push(event{typ: evTrack, kind: KindTrack, dir: dir, track: t, pending: pending})
}

// CorrectTrack fixes the details on a card that is still on screen. If the
// card has already gone, the correction is dropped rather than reviving it:
// a finished card reappearing to call a track "next" while it is playing is
// worse than never correcting it at all.
func (o *OSD) CorrectTrack(dir Direction, t Track) {
	o.push(event{typ: evTrack, kind: KindTrack, dir: dir, track: t, correct: true})
}

func (o *OSD) push(e event) {
	if o == nil || !o.opts.Enabled {
		return
	}
	select {
	case o.events <- e:
	default:
	}
}

// frameInterval is how long one frame is allowed to take.
func (o *OSD) frameInterval() time.Duration {
	fps := o.opts.FPS
	if fps <= 0 {
		fps = refreshRate()
	}
	if fps < minFPS {
		fps = minFPS
	}
	if fps > maxFPS {
		fps = maxFPS
	}
	return time.Second / time.Duration(fps)
}

// Run creates the window and drives the animation until ctx is cancelled. It
// runs even when the overlay is disabled, so that switching it on in the
// config takes effect without a restart; a disabled overlay simply drops
// every event and never shows the window.
func (o *OSD) Run(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	win, err := newWindow(int(o.layout.canvasW), int(o.layout.canvasH))
	if err != nil {
		return err
	}
	defer win.destroy()

	o.frameDur = o.frameInterval()
	o.log.Info("osd ready",
		"scale", o.layout.scale,
		"fps", int(time.Second/o.frameDur),
		"size", [2]int{int(o.layout.canvasW), int(o.layout.canvasH)})

	deadline := time.Now()
	var frames int
	tally := time.Now()

	for {
		if o.ph == hidden {
			// Nothing on screen: sleep on the queue, waking often enough to
			// keep the window's message queue serviced.
			select {
			case <-ctx.Done():
				return nil
			case e := <-o.events:
				o.apply(e, ctx)
				deadline = time.Now()
			case <-time.After(idleGap):
				win.pump()
				continue
			}
		}

		// Drain anything else that queued up in the same instant.
		for drained := false; !drained; {
			select {
			case e := <-o.events:
				o.apply(e, ctx)
			default:
				drained = true
			}
		}

		if o.needRebuild {
			o.needRebuild = false
			win.destroy()
			w, err := newWindow(int(o.layout.canvasW), int(o.layout.canvasH))
			if err != nil {
				return fmt.Errorf("rebuild overlay window: %w", err)
			}
			win = w
			o.shown, o.haveFrame = false, false
			o.log.Info("osd reconfigured", "scale", o.layout.scale,
				"fps", int(time.Second/o.frameDur), "position", o.opts.Position)
		}

		win.pump()
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		now := time.Now()
		x, y := o.origin()
		o.checkFullscreen(now)
		o.checkDismiss(now, func() bool {
			return mouseClicked() && o.cardRect(x, y).contains(cursorPos())
		})
		st, tr, visible := o.advance(now)
		if !visible {
			win.hide()
			o.shown = false
			continue
		}

		// Recompose only when the card's content actually changed.
		key := st.key(o.layout.contentW, o.layout.cardW, o.queueRev)
		if !o.haveFrame || key != o.frameKey {
			o.renderFrame(st)
			o.frameKey = key
			o.haveFrame = true
			o.shown = false
		}

		// A window switch is when the compositor is most likely to drop what
		// it is holding for a layered window. Take the overlay down and put
		// it back, which forces the surface to be rebuilt from scratch; the
		// pixels go out before it is shown again, so nothing blank is ever
		// on screen.
		// A window switch is when the compositor is most likely to be holding
		// something stale for a layered window, so push a fresh frame and put
		// the overlay back on top. Note what is *not* done here: hiding and
		// re-showing the window. That was tried, and it is what produces the
		// opaque black rectangle - rebuilding the surface mid-transition
		// composites it without alpha. Measured at 2 failures in 8 switches
		// with the hide, 0 in 25 without it.
		switched := false
		if fg := foregroundWindow(); fg != o.lastForeground {
			o.lastForeground = fg
			o.shown = false
			switched = true
		}

		// Re-push the pixels when anything moved, and periodically even when
		// nothing did: a window switch can leave the compositor holding stale
		// or empty content for a layered window, and there is no notification
		// when that happens.
		pos := [2]int{x, y}
		stale := now.Sub(o.lastPresent) > refreshEvery
		if !o.shown || stale || tr != o.shownTr || pos != o.shownPos {
			if err := win.present(o.paint.dst, x, y, tr.offsetY, tr.opacity); err != nil {
				// Do not record it as shown; a failed update leaves the window
				// holding whatever was there before, so retry next frame.
				o.log.Warn("overlay update failed, retrying", "err", err)
				o.shown = false
			} else {
				wasShown := o.shown
				o.shown, o.shownTr, o.shownPos = true, tr, pos
				o.lastPresent = now
				if switched || !wasShown {
					win.reassertTopmost()
				}
			}
		}

		// Report what is actually being achieved, so "is it smooth?" has an
		// answer that is not a guess.
		frames++
		if d := time.Since(tally); d >= time.Second {
			o.log.Debug("osd frame rate", "fps", float64(frames)/d.Seconds())
			frames, tally = 0, time.Now()
		}

		// Pace to the target frame time without drifting.
		frameDur := o.frameDur
		deadline = deadline.Add(frameDur)
		if d := time.Until(deadline); d > 0 {
			time.Sleep(d)
		} else if time.Since(deadline) > frameDur {
			deadline = time.Now() // fell far behind, resynchronise
		}
	}
}

// checkDismiss retires the card as soon as the user clicks anything. Reaching
// for the mouse means the card has been read and is now in the way.
// cardBox is where the card sits on screen, used to decide whether a click
// landed on it.
type cardBox struct{ x0, y0, x1, y1 int }

func (b cardBox) contains(x, y int) bool {
	return x >= b.x0 && x < b.x1 && y >= b.y0 && y < b.y1
}

// cardRect is the card's screen rectangle for a window drawn at (winX, winY).
// It excludes the transparent padding the canvas carries for its shadow, so a
// click just outside the visible panel does not count as a click on it.
func (o *OSD) cardRect(winX, winY int) cardBox {
	l := o.layout
	ch := l.cardH
	if o.content.kind == KindPeek {
		ch = l.peekH
	}
	x := winX + int(l.cardX)
	y := winY + int(l.cardTop(ch))
	return cardBox{x, y, x + int(l.cardW), y + int(ch)}
}

// clicked is supplied by the caller so the decision can be tested without a
// mouse; the real one reports a press that landed on the card itself.
//
// Only clicks on the card dismiss it. Dismissing on any click anywhere makes
// the overlay useless in a game, where the mouse is being clicked constantly
// and the card would vanish the instant it appeared.
func (o *OSD) checkDismiss(now time.Time, clicked func() bool) {
	if o.ph == hidden || o.ph == exiting {
		return
	}
	if o.primeClick {
		// A button already held when the card appeared is not a dismissal.
		clicked()
		o.primeClick = false
		return
	}
	if !o.opts.DismissOnClick {
		return
	}
	if clicked() {
		o.ph = exiting
		o.phaseStart = now
	}
}

// checkFullscreen retires the card when something goes full-screen while it
// is on display. The query is throttled: it is cheap, but not 144 times a
// second cheap.
func (o *OSD) checkFullscreen(now time.Time) {
	if !o.opts.HideFullscreen || o.ph == hidden || o.ph == exiting {
		return
	}
	if now.Sub(o.lastFSCheck) < 250*time.Millisecond {
		return
	}
	o.lastFSCheck = now
	if fullscreenActive() {
		o.ph = exiting
		o.phaseStart = now
	}
}

// apply folds an incoming event into the animation state.
func (o *OSD) apply(e event, ctx context.Context) {
	now := time.Now()

	switch e.typ {
	case evArt:
		// Late-arriving cover for the track we are currently showing.
		if e.url != o.wantURL || e.art == nil {
			return
		}
		o.content.art = e.art
		o.content.accent = e.art.accent
		o.lastAccent = e.art.accent
		o.artFadeStart = now
		return

	case evQueue:
		o.queue = e.queue
		o.queueRev++
		if o.selected >= len(o.queue) {
			o.selected = max(0, len(o.queue)-1)
		}
		// Warm the row covers now so opening the peek is not a wall of
		// placeholders.
		for _, t := range o.queue {
			if t.ArtURL == "" || o.art.get(t.ArtURL) != nil {
				continue
			}
			url := t.ArtURL
			o.art.fetch(ctx, url, func(a *artwork) {
				o.push(event{typ: evArt, url: url, art: a})
			})
		}
		return

	case evConfig:
		o.applyConfig(e.opts)
		return

	case evPeek:
		if o.opts.HideFullscreen && o.ph == hidden && fullscreenActive() {
			return
		}
		o.content.kind = KindPeek
		o.content.pending = false
		o.selected = e.selected
		if o.selected < 0 {
			o.selected = 0
		}
		if o.selected >= len(o.queue) {
			o.selected = max(0, len(o.queue)-1)
		}
		hold := e.hold
		if hold <= 0 {
			hold = 400 * time.Millisecond
		}
		o.holdUntil = now.Add(hold)

	case evNotice:
		if o.opts.HideFullscreen && o.ph == hidden && fullscreenActive() {
			return
		}
		o.content.kind = KindNotice
		o.content.pending = false
		o.content.label = e.label
		// The message is the headline; the track it is about is the subtitle,
		// which is the same reading order the track card uses.
		o.content.title = e.url
		o.content.artist = e.track.Title
		o.setArt(ctx, e.track.ArtURL)
		o.setTiming(e.track)
		o.holdUntil = now.Add(o.opts.TrackHold)

	case evVolume:
		if o.opts.HideFullscreen && o.ph == hidden && fullscreenActive() {
			return
		}
		o.content.kind = KindVolume
		o.content.volume = e.volume
		o.content.pending = false
		if !e.track.empty() {
			o.content.title = e.track.Title
			o.content.artist = e.track.Artist
		}
		o.setArt(ctx, e.track.ArtURL)
		o.setTiming(e.track)
		o.holdUntil = now.Add(o.opts.VolumeHold)

	case evTrack:
		if e.correct {
			// Only worth anything while the card is up.
			if o.ph == hidden || o.ph == exiting || o.content.kind != KindTrack {
				return
			}
			o.content.pending = false
			o.content.title = e.track.Title
			o.content.artist = e.track.Artist
			o.setArt(ctx, e.track.ArtURL)
			o.setTiming(e.track)
			return
		}
		if o.opts.HideFullscreen && o.ph == hidden && fullscreenActive() {
			return
		}
		o.content.kind = KindTrack
		o.content.dir = e.dir
		o.content.pending = e.pending
		o.content.title = e.track.Title
		o.content.artist = e.track.Artist
		o.setArt(ctx, e.track.ArtURL)
		o.setTiming(e.track)
		o.holdUntil = now.Add(o.opts.TrackHold)
	}

	switch o.ph {
	case hidden:
		o.ph = entering
		o.phaseStart = now
		o.lastAdvance = now
		o.primeClick = true
		o.barCurrent = float64(o.content.volume) / 100
	case exiting:
		// Interrupt the fade-out without popping: rewind into the entry
		// animation from wherever the opacity currently is.
		p := 1 - progress(now, o.phaseStart, exitDur)
		o.ph = entering
		o.phaseStart = now.Add(-time.Duration(easeInverseEnter(p) * float64(enterDur)))
	case entering, holding:
		// Keep animating, just extend the hold.
	}
}

// setTiming remembers where the playhead was, so the progress line can be
// extrapolated between polls instead of asking Spotify every frame.
func (o *OSD) setTiming(t Track) {
	if t.Duration <= 0 {
		// Nothing known about this track's length. Clear the reading rather
		// than leaving the previous track's playhead in place, which would
		// draw a progress line belonging to a song that is no longer on.
		o.curDuration, o.curPosition, o.curPlaying = 0, 0, false
		o.curPositionAt = time.Time{}
		return
	}
	o.curDuration = t.Duration
	o.curPosition = t.Position
	o.curPositionAt = t.PositionAt
	if o.curPositionAt.IsZero() {
		o.curPositionAt = time.Now()
	}
	o.curPlaying = t.Playing
}

// applyConfig folds a reloaded config into the running overlay. Geometry
// changes need the window rebuilt; everything else takes effect immediately.
func (o *OSD) applyConfig(opts Options) {
	geometry := opts.Scale != o.opts.Scale || opts.Position != o.opts.Position
	o.opts = opts
	o.frameDur = o.frameInterval()

	if !geometry {
		return
	}
	o.layout = newLayout(opts.Scale*displayScale(), opts.Position)
	o.paint = newPainter(int(o.layout.canvasW), int(o.layout.canvasH))
	o.art = newArtCache(int(o.layout.artSize), int(o.layout.peekThumb))
	o.shadow, o.barGlow = maskCache{}, maskCache{}
	o.content.art = nil
	o.wantURL = ""
	o.needRebuild = true
}

// setArt points the card at a cover, pulling it from cache or fetching it.
func (o *OSD) setArt(ctx context.Context, url string) {
	if url == "" {
		o.wantURL = ""
		o.content.art = nil
		o.content.accent = o.lastAccent
		return
	}
	if url == o.wantURL && o.content.art != nil {
		return
	}
	o.wantURL = url
	if a := o.art.get(url); a != nil {
		o.content.art = a
		o.content.accent = a.accent
		o.lastAccent = a.accent
		o.artFadeStart = time.Now()
		return
	}
	// Not cached: show the placeholder in the previous accent, and swap the
	// cover in when it lands.
	o.content.art = nil
	o.content.accent = o.lastAccent
	o.art.fetch(ctx, url, func(a *artwork) {
		o.push(event{typ: evArt, url: url, art: a})
	})
}

// advance computes the frame for now: the card's content, the transform to
// apply to it, and whether anything is visible at all.
func (o *OSD) advance(now time.Time) (frameState, transform, bool) {
	st := o.content
	var tr transform

	switch o.ph {
	case entering:
		e := easeOutQuint(progress(now, o.phaseStart, enterDur))
		tr.opacity = e
		tr.offsetY = o.layout.slide * (1 - e)
		if e >= 1 {
			o.ph = holding
			o.phaseStart = now
		}
	case holding:
		tr.opacity = 1
		tr.offsetY = 0
		if now.After(o.holdUntil) {
			o.ph = exiting
			o.phaseStart = now
		}
	case exiting:
		p := progress(now, o.phaseStart, exitDur)
		e := easeInQuad(p)
		tr.opacity = 1 - e
		tr.offsetY = o.layout.slide * 0.5 * e
		if p >= 1 {
			o.ph = hidden
			return st, tr, false
		}
	default:
		return st, tr, false
	}

	// The bar glides toward the target rather than snapping, at a rate that
	// does not depend on the frame rate.
	dt := now.Sub(o.lastAdvance)
	o.lastAdvance = now
	if dt > 0 {
		target := clamp01(float64(st.volume) / 100)
		a := 1 - math.Exp(-float64(dt)/float64(barTau))
		o.barCurrent += (target - o.barCurrent) * a
		if math.Abs(target-o.barCurrent) < 0.0005 {
			o.barCurrent = target
		}
	}
	st.bar = o.barCurrent

	if o.curDuration > 0 {
		pos := o.curPosition
		if o.curPlaying {
			pos += now.Sub(o.curPositionAt)
		}
		if pos > o.curDuration {
			pos = o.curDuration
		}
		st.progress = clamp01(float64(pos) / float64(o.curDuration))
		st.elapsed, st.total = pos, o.curDuration
	}

	if st.kind == KindPeek {
		st.queue = o.queue
		st.selected = o.selected
		if cap(o.queueArtBuf) < len(o.queue) {
			o.queueArtBuf = make([]*artwork, len(o.queue))
		}
		st.queueArt = o.queueArtBuf[:len(o.queue)]
		loaded := 0
		for i, t := range o.queue {
			st.queueArt[i] = o.art.get(t.ArtURL)
			if st.queueArt[i] != nil {
				loaded++
			}
		}
		st.artLoaded = loaded
	}

	st.artFade = 1
	if st.art != nil && !o.artFadeStart.IsZero() {
		st.artFade = easeOutQuint(progress(now, o.artFadeStart, artFade))
	}
	if st.accent.A == 0 {
		st.accent = colAccentFallback
	}
	return st, tr, true
}

// origin places the window so the card lands the requested distance from the
// edge of the work area, accounting for the transparent padding the canvas
// carries around the card for its shadow.
func (o *OSD) origin() (int, int) {
	l := o.layout
	wa := workArea()
	margin := l.px(44)

	cardTop := l.cardY
	cardBottom := l.cardBottom()
	left, top := float64(wa.Left), float64(wa.Top)
	right, bottom := float64(wa.Right), float64(wa.Bottom)

	centerX := left + (right-left-l.canvasW)/2

	switch o.opts.Position {
	case "top-center":
		return int(centerX), int(top + margin - cardTop)
	case "bottom-right":
		return int(right - l.canvasW - margin + l.px(basePadX)), int(bottom - margin - cardBottom)
	case "top-right":
		return int(right - l.canvasW - margin + l.px(basePadX)), int(top + margin - cardTop)
	default: // bottom-center
		return int(centerX), int(bottom - margin - cardBottom)
	}
}

func progress(now, start time.Time, d time.Duration) float64 {
	if d <= 0 {
		return 1
	}
	return clamp01(float64(now.Sub(start)) / float64(d))
}

// easeOutQuint decelerates hard at the end. At a high refresh rate the extra
// smoothness over a cubic is actually visible.
func easeOutQuint(t float64) float64 {
	t = clamp01(t)
	u := 1 - t
	return 1 - u*u*u*u*u
}

func easeInQuad(t float64) float64 {
	t = clamp01(t)
	return t * t
}

// easeInverseEnter maps an opacity back to the entry progress that produces
// it, so an interrupted fade-out can resume from exactly where it is.
func easeInverseEnter(opacity float64) float64 {
	return 1 - math.Pow(1-clamp01(opacity), 1.0/5.0)
}
