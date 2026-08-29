package osd

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/image/font"
)

// The lyrics panel: a floating, always-on-top window that follows the
// playhead line by line.
//
// It is deliberately a separate window from the overlay card rather than
// another card kind. The card is a notification - it appears, says one thing
// and leaves, and must never take a click. This is a companion the user
// leaves open, moves where they want it and resizes; the two have opposite
// requirements almost everywhere they touch the window manager.

// LyricLine is one line of a song and the moment it begins.
type LyricLine struct {
	At   time.Duration
	Text string
}

// LyricDoc is a whole song's words, as handed to the panel.
type LyricDoc struct {
	Lines        []LyricLine
	Synced       bool
	Instrumental bool
	Source       string
}

// LyricsTrack is what the panel needs to know about the current track.
type LyricsTrack struct {
	Title      string
	Artist     string
	URI        string
	ArtURL     string
	Duration   time.Duration
	Position   time.Duration
	PositionAt time.Time
	Playing    bool
}

// docState is what the body shows when there is no text to show.
type docState int

const (
	docIdle    docState = iota // nothing asked for yet
	docLoading                 // a lookup is in flight
	docReady                   // Lines are worth drawing
	docMissing                 // looked, found nothing
	docFailed                  // the lookup itself broke
)

// LyricsOptions configures the panel.
type LyricsOptions struct {
	Enabled bool
	Opacity float64 // whole-window alpha, 0.5-1
	Scale   float64 // display scale; 0 asks the system
	FPS     int     // 0 follows the monitor

	// Geometry in physical pixels. Zero width or height means "pick a
	// sensible default on the monitor under the mouse".
	X, Y, W, H int

	// OnGeometry is called after the user finishes a move or resize, so the
	// caller can remember where the panel was left.
	OnGeometry func(x, y, w, h int)

	// OnSeek is called when the user drops the progress rail's handle, with
	// the position they dropped it at. It may block: the panel calls it on
	// its own goroutine.
	OnSeek func(pos time.Duration)

	// OnOpacity is called when the user lets go of the opacity slider, so the
	// caller can remember how see-through they wanted the panel.
	OnOpacity func(v float64)
}

// Lyrics panel geometry, in logical pixels before the display scale.
const (
	lyrDefaultW = 430
	lyrDefaultH = 540
	lyrMinW     = 300
	lyrMinH     = 220
	lyrMaxW     = 900
	lyrMaxH     = 1100

	lyrMargin   = 5 // transparent gutter for the outer ring
	lyrRadius   = 20
	lyrHeaderH  = 76
	lyrFooterH  = 44
	lyrBodyPadX = 26
	lyrThumb    = 46
	lyrGripSize = 22
	lyrEdge     = 7 // grab band along the right and bottom edges

	// The opacity slider lives in the header, under the close button.
	lyrSliderW    = 80
	lyrSliderH    = 3
	lyrSliderKnob = 5.5
	// Below this the panel stops being readable over a bright background, so
	// the slider will not go there and neither will the config.
	lyrOpacityMin = 0.40

	lyrLineSize    = 19
	lyrLineGap     = 11   // between wrapped paragraphs
	lyrLineLead    = 1.28 // multiple of the font size, within a paragraph
	lyrActiveBarW  = 3
	lyrFadeLines   = 1.6 // how many line heights the top/bottom fade covers
	lyrAnchor      = 0.36
	lyrScrollTau   = 150 * time.Millisecond
	lyrManualHold  = 5 * time.Second
	lyrWheelStep   = 54
	lyrIdleGap     = 60 * time.Millisecond
	lyrTopmostFreq = 2 * time.Second
)

// zone is what the pointer is over.
type zone int

const (
	zoneNone zone = iota
	zoneHeader
	zoneBody
	zoneClose
	zoneGripCorner
	zoneGripBottom
	zoneRail
	zoneOpacity
)

// dragMode is what a held mouse button is currently doing.
type dragMode int

const (
	dragNone dragMode = iota
	dragMove
	dragResize
	dragScroll
	dragSeek
	dragOpacity
)

type lyricsCmdKind int

const (
	lcToggle lyricsCmdKind = iota
	lcShow
	lcHide
	lcTrack
	lcDoc
	lcConfig
)

type lyricsCmd struct {
	kind  lyricsCmdKind
	track LyricsTrack
	doc   *LyricDoc
	key   string // track URI the doc belongs to
	state docState
	opts  LyricsOptions
}

// LyricsWindow is the panel. Everything except the command channel is touched
// only by the goroutine running Run.
type LyricsWindow struct {
	log    *slog.Logger
	events chan lyricsCmd

	opts   LyricsOptions
	scale  float64
	fonts  *fontSet
	paint  *painter
	art    *artCache
	win    *lyricsWin
	visibl atomic.Bool

	track   LyricsTrack
	doc     *LyricDoc
	docKey  string
	state   docState
	accent  color.RGBA
	artURL  string
	lastArt *artwork

	// Layout of the wrapped text, rebuilt when the doc or the width changes.
	para      []paragraph
	wrapW     float64
	wrapKey   string
	contentH  float64
	lineH     float64
	scroll    float64
	scrollTo  float64
	manualTil time.Time
	active    int

	// Scrubbing the progress rail. seekHold is how long the position the user
	// dropped the handle at outranks whatever the daemon reports: a poll can
	// still be carrying the pre-seek playhead, and letting it win would snap
	// the rail back under the cursor.
	scrubbing bool
	scrubPos  time.Duration
	seekHold  time.Time
	railHot   bool

	// The opacity slider. sliderShow keeps the percentage readout up for a
	// moment after the drag ends, so the number does not vanish the instant
	// you let go of it.
	sliderHot  bool
	sliding    bool
	sliderShow time.Time

	// Input state.
	drag        dragMode
	dragX       int
	dragY       int
	dragW       int
	dragH       int
	hover       zone
	pointer     zone
	closeHot    bool
	lastPresent time.Time
	lastTopmost time.Time
	frameDur    time.Duration
	dirty       bool
}

// paragraph is one lyric line after wrapping, with its own vertical extent.
type paragraph struct {
	at    time.Duration
	rows  []string
	top   float64
	h     float64
	blank bool
}

// NewLyrics builds the panel. It does nothing until Run is called.
func NewLyrics(opts LyricsOptions, log *slog.Logger) *LyricsWindow {
	return &LyricsWindow{
		log:    log,
		opts:   opts,
		events: make(chan lyricsCmd, 32),
		state:  docIdle,
		active: -1,
	}
}

// Toggle opens the panel, or closes it if it is already open.
func (w *LyricsWindow) Toggle() { w.push(lyricsCmd{kind: lcToggle}) }

// Show opens the panel.
func (w *LyricsWindow) Show() { w.push(lyricsCmd{kind: lcShow}) }

// Hide closes it.
func (w *LyricsWindow) Hide() { w.push(lyricsCmd{kind: lcHide}) }

// Visible reports whether the panel is on screen. It is safe from any
// goroutine, which is what lets the fetcher decide whether to bother.
func (w *LyricsWindow) Visible() bool { return w.visibl.Load() }

// SetTrack tells the panel what is playing and where the playhead is.
func (w *LyricsWindow) SetTrack(t LyricsTrack) {
	w.push(lyricsCmd{kind: lcTrack, track: t})
}

// SetDoc hands over the words for a track, or the reason there are none.
// key must be the track the lyrics belong to; a doc that arrives after the
// song has moved on is dropped rather than shown against the wrong track.
func (w *LyricsWindow) SetDoc(key string, doc *LyricDoc, state docState) {
	w.push(lyricsCmd{kind: lcDoc, key: key, doc: doc, state: state})
}

// Loading marks the panel as waiting on a lookup for key.
func (w *LyricsWindow) Loading(key string) { w.SetDoc(key, nil, docLoading) }

// Ready marks lyrics as available.
func (w *LyricsWindow) Ready(key string, doc *LyricDoc) { w.SetDoc(key, doc, docReady) }

// Missing marks a track as having no lyrics.
func (w *LyricsWindow) Missing(key string) { w.SetDoc(key, nil, docMissing) }

// Failed marks a lookup that broke, as opposed to one that found nothing.
func (w *LyricsWindow) Failed(key string) { w.SetDoc(key, nil, docFailed) }

// Reconfigure adopts a reloaded config.
func (w *LyricsWindow) Reconfigure(opts LyricsOptions) {
	w.push(lyricsCmd{kind: lcConfig, opts: opts})
}

func (w *LyricsWindow) push(c lyricsCmd) {
	select {
	case w.events <- c:
	default:
		// Never block a caller on the panel; a dropped repaint request is
		// invisible, and the next one is 60ms away.
	}
}

// Run owns the panel's window and message pump until ctx is cancelled.
func (w *LyricsWindow) Run(ctx context.Context) error {
	if !w.opts.Enabled {
		<-ctx.Done()
		return nil
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w.scale = w.opts.Scale
	if w.scale <= 0 {
		w.scale = displayScale()
	}
	w.fonts = newFontSet()
	w.frameDur = w.frameInterval()

	maxW, maxH := int(w.px(lyrMaxW)), int(w.px(lyrMaxH))
	w.paint = newPainter(maxW, maxH)
	w.art = newArtCache(int(w.px(lyrThumb)), int(w.px(lyrThumb)))

	x, y, ww, hh := w.startGeometry()
	win, err := newLyricsWin(maxW, maxH, ww, hh, x, y)
	if err != nil {
		return err
	}
	w.win = win
	lyricsOwner = w
	defer func() {
		lyricsOwner = nil
		win.destroy()
	}()

	w.log.Info("lyrics panel ready",
		"scale", w.scale, "size", [2]int{ww, hh}, "at", [2]int{x, y},
		"fps", int(time.Second/w.frameDur))

	for {
		if !w.win.visible {
			select {
			case <-ctx.Done():
				return nil
			case c := <-w.events:
				w.apply(ctx, c)
			case <-time.After(lyrIdleGap):
				w.win.pump()
			}
			continue
		}

		for drained := false; !drained; {
			select {
			case c := <-w.events:
				w.apply(ctx, c)
			default:
				drained = true
			}
		}

		// One of those events may have closed the panel. Falling through would
		// render and present a frame, and present shows the window as part of
		// putting pixels in it - painting the panel straight back onto the
		// screen the user just dismissed it from.
		if !w.win.visible {
			continue
		}
		w.win.pump()

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		now := time.Now()
		w.advance(now)
		if w.dirty {
			w.render()
			if err := w.win.present(w.paint.dst, w.opacity()); err != nil {
				w.log.Debug("lyrics present failed", "err", err)
			}
			w.dirty = false
		}
		if now.Sub(w.lastTopmost) > lyrTopmostFreq {
			w.lastTopmost = now
			w.win.reassertTopmost()
		}

		if sleep := w.frameDur - time.Since(now); sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

func (w *LyricsWindow) px(v float64) float64 { return v * w.scale }

func (w *LyricsWindow) opacity() float64 {
	o := w.opts.Opacity
	if o <= 0 {
		return 0.95
	}
	return clampOpacity(o)
}

// clampOpacity keeps a value inside the range the slider offers, so a config
// file and a dragged handle cannot disagree about what is possible.
func clampOpacity(v float64) float64 {
	return math.Min(1, math.Max(lyrOpacityMin, v))
}

// opacityAt maps a pointer position along the slider onto an opacity, and
// opacityFraction is its inverse for drawing the fill. Both are free functions
// so the mapping can be tested without a window.
func opacityAt(x, sliderX, sliderW float64) float64 {
	if sliderW <= 0 {
		return 1
	}
	f := (x - sliderX) / sliderW
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	return lyrOpacityMin + f*(1-lyrOpacityMin)
}

func opacityFraction(v float64) float64 {
	return (clampOpacity(v) - lyrOpacityMin) / (1 - lyrOpacityMin)
}

func (w *LyricsWindow) frameInterval() time.Duration {
	fps := w.opts.FPS
	if fps <= 0 {
		fps = refreshRate()
	}
	if fps < 24 {
		fps = 24
	}
	if fps > 144 {
		fps = 144
	}
	return time.Second / time.Duration(fps)
}

// startGeometry is where the panel opens: whatever was saved, otherwise the
// right-hand side of the monitor under the pointer, clear of the taskbar.
func (w *LyricsWindow) startGeometry() (x, y, ww, hh int) {
	ww, hh = w.opts.W, w.opts.H
	if ww <= 0 || hh <= 0 {
		ww, hh = int(w.px(lyrDefaultW)), int(w.px(lyrDefaultH))
	}
	ww = clampi(ww, int(w.px(lyrMinW)), int(w.px(lyrMaxW)))
	hh = clampi(hh, int(w.px(lyrMinH)), int(w.px(lyrMaxH)))

	if w.opts.W > 0 && w.opts.H > 0 {
		x, y = w.opts.X, w.opts.Y
		return x, y, ww, hh
	}
	cx, cy := cursorPos()
	l, t, r, b := workAreaAt(cx, cy)
	x = r - ww - int(w.px(32))
	y = t + (b-t-hh)/2
	if x < l {
		x = l
	}
	return x, y, ww, hh
}

func (w *LyricsWindow) apply(ctx context.Context, c lyricsCmd) {
	switch c.kind {
	case lcToggle:
		if w.win.visible {
			w.close()
		} else {
			w.open()
		}
	case lcShow:
		w.open()
	case lcHide:
		w.close()
	case lcConfig:
		keep := w.opts
		w.opts = c.opts
		w.opts.X, w.opts.Y, w.opts.W, w.opts.H = keep.X, keep.Y, keep.W, keep.H
		w.opts.OnGeometry = keep.OnGeometry
		if w.opts.OnSeek == nil {
			w.opts.OnSeek = keep.OnSeek
		}
		w.frameDur = w.frameInterval()
		w.dirty = true
	case lcTrack:
		w.setTrack(ctx, c.track)
	case lcDoc:
		if c.key != "" && w.track.URI != "" && c.key != w.track.URI {
			return // the song moved on while the lookup was in flight
		}
		w.doc, w.docKey, w.state = c.doc, c.key, c.state
		w.wrapKey = ""
		w.scroll, w.scrollTo, w.active = 0, 0, -1
		w.manualTil = time.Time{}
		w.dirty = true
	}
}

func (w *LyricsWindow) open() {
	w.visibl.Store(true)
	w.dirty = true
	w.lastTopmost = time.Time{}
	// A present is what actually shows the window; showing first would put an
	// uninitialised black rectangle on screen for a frame.
	w.render()
	if err := w.win.present(w.paint.dst, w.opacity()); err != nil {
		w.log.Warn("lyrics panel could not be shown", "err", err)
	}
	w.win.reassertTopmost()
}

func (w *LyricsWindow) close() {
	w.visibl.Store(false)
	w.drag = dragNone
	w.dirty = false
	releaseMouse()
	w.win.hide()
}

func (w *LyricsWindow) setTrack(ctx context.Context, t LyricsTrack) {
	changed := t.URI != w.track.URI || t.Title != w.track.Title

	// A reading that predates the seek the user just made is worse than no
	// reading at all, so keep ours until the write has had time to land.
	if !changed && time.Now().Before(w.seekHold) {
		t.Position, t.PositionAt = w.track.Position, w.track.PositionAt
	}
	w.track = t
	if changed {
		w.doc, w.state = nil, docIdle
		w.wrapKey = ""
		w.scroll, w.scrollTo, w.active = 0, 0, -1
		w.manualTil = time.Time{}
	}
	if t.ArtURL != "" && t.ArtURL != w.artURL {
		w.artURL = t.ArtURL
		if a := w.art.get(t.ArtURL); a != nil {
			w.lastArt, w.accent = a, a.accent
		} else {
			url := t.ArtURL
			w.art.fetch(ctx, url, func(a *artwork) {
				w.push(lyricsCmd{kind: lcTrack, track: w.trackWithArt(url)})
			})
		}
	}
	if a := w.art.get(w.artURL); a != nil {
		w.lastArt, w.accent = a, a.accent
	}
	w.dirty = true
}

// trackWithArt re-sends the current track once its cover has landed, so the
// panel repaints with the real artwork and its accent.
func (w *LyricsWindow) trackWithArt(url string) LyricsTrack {
	t := w.track
	t.ArtURL = url
	return t
}

// seekHoldFor is how long a dropped handle outranks the daemon's reading. It
// covers one poll round-trip with room to spare.
const seekHoldFor = 2500 * time.Millisecond

// position is the live playhead, extrapolated from the last reading. While the
// rail is being dragged it is wherever the handle is, so the highlighted lyric
// follows the scrub instead of the music.
func (w *LyricsWindow) position(now time.Time) time.Duration {
	if w.scrubbing {
		return w.scrubPos
	}
	pos := w.track.Position
	if w.track.Playing && !w.track.PositionAt.IsZero() {
		pos += now.Sub(w.track.PositionAt)
	}
	if w.track.Duration > 0 && pos > w.track.Duration {
		pos = w.track.Duration
	}
	if pos < 0 {
		pos = 0
	}
	return pos
}

// advance moves the highlight and eases the scroll toward it.
func (w *LyricsWindow) advance(now time.Time) {
	pos := w.position(now)

	active := -1
	if w.doc != nil && w.doc.Synced {
		active = indexAt(w.doc.Lines, pos)
	}
	if active != w.active {
		w.active = active
		w.dirty = true
	}

	w.layout()

	if w.doc != nil && w.doc.Synced && now.After(w.manualTil) {
		w.scrollTo = w.anchorFor(active)
	}
	w.scrollTo = w.clampScroll(w.scrollTo)

	dt := now.Sub(w.lastPresent)
	w.lastPresent = now
	if dt <= 0 || dt > 250*time.Millisecond {
		dt = w.frameDur
	}
	if d := w.scrollTo - w.scroll; math.Abs(d) > 0.05 {
		w.scroll += d * (1 - math.Exp(-float64(dt)/float64(lyrScrollTau)))
		w.dirty = true
	} else if w.scroll != w.scrollTo {
		w.scroll = w.scrollTo
		w.dirty = true
	}

	// The footer clock ticks once a second even when nothing else moves.
	if w.track.Duration > 0 {
		w.dirty = w.dirty || int(pos/time.Second) != int((pos-w.frameDur)/time.Second)
	}
}

// anchorFor is the scroll offset that puts line i at the reading anchor.
func (w *LyricsWindow) anchorFor(i int) float64 {
	if i < 0 || i >= len(w.para) {
		return 0
	}
	_, _, _, bodyH := w.bodyRect()
	return w.para[i].top - bodyH*lyrAnchor
}

func (w *LyricsWindow) clampScroll(v float64) float64 {
	_, _, _, bodyH := w.bodyRect()
	max := w.contentH - bodyH*0.75 // let the last line rise clear of the footer
	if max < 0 {
		max = 0
	}
	return math.Min(math.Max(v, 0), max)
}

// seekTarget maps a pointer position along the rail onto a position in the
// track. Kept free of the window so the arithmetic can be tested on its own.
func seekTarget(x, railX, railW float64, dur time.Duration) time.Duration {
	if railW <= 0 || dur <= 0 {
		return 0
	}
	f := (x - railX) / railW
	if f < 0 {
		f = 0
	} else if f > 1 {
		f = 1
	}
	return time.Duration(f * float64(dur))
}

func indexAt(lines []LyricLine, pos time.Duration) int {
	lo, hi := 0, len(lines)
	for lo < hi {
		mid := (lo + hi) / 2
		if lines[mid].At <= pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

// bodyRect is the scrolling text area, in window coordinates.
func (w *LyricsWindow) bodyRect() (x, y, ww, hh float64) {
	m := w.px(lyrMargin)
	x = m + w.px(lyrBodyPadX)
	y = m + w.px(lyrHeaderH)
	ww = float64(w.win.w) - 2*m - 2*w.px(lyrBodyPadX)
	hh = float64(w.win.h) - m - w.px(lyrHeaderH) - w.px(lyrFooterH) - m
	if hh < 0 {
		hh = 0
	}
	return x, y, ww, hh
}

// layout wraps the lyrics to the current width. It is a no-op unless the doc
// or the width actually changed, so resizing is the only thing that pays.
func (w *LyricsWindow) layout() {
	_, _, bodyW, _ := w.bodyRect()
	key := fmt.Sprintf("%s|%d|%.0f", w.docKey, len(linesOf(w.doc)), bodyW)
	if key == w.wrapKey {
		return
	}
	w.wrapKey = key
	w.wrapW = bodyW
	w.para = w.para[:0]

	face := w.fonts.face(semibold, w.px(lyrLineSize))
	w.lineH = w.px(lyrLineSize * lyrLineLead)
	gap := w.px(lyrLineGap)

	top := 0.0
	for _, ln := range linesOf(w.doc) {
		p := paragraph{at: ln.At, top: top, blank: strings.TrimSpace(ln.Text) == ""}
		if p.blank {
			// A rest between verses. It still gets a slot so the highlight can
			// sit on it, just a short one.
			p.h = w.lineH * 0.8
		} else {
			p.rows = wrapText(face, ln.Text, bodyW)
			p.h = float64(len(p.rows)) * w.lineH
		}
		w.para = append(w.para, p)
		top += p.h + gap
	}
	w.contentH = top
}

func linesOf(d *LyricDoc) []LyricLine {
	if d == nil {
		return nil
	}
	return d.Lines
}

// wrapText breaks s into rows no wider than maxW, splitting inside a word
// only when a single word cannot fit on its own.
func wrapText(face font.Face, s string, maxW float64) []string {
	s = strings.TrimSpace(s)
	if s == "" || face == nil || maxW <= 0 {
		return []string{s}
	}
	if measure(face, s) <= maxW {
		return []string{s}
	}

	var rows []string
	line := ""
	flush := func() {
		if line != "" {
			rows = append(rows, line)
			line = ""
		}
	}
	for _, word := range strings.Fields(s) {
		candidate := word
		if line != "" {
			candidate = line + " " + word
		}
		if measure(face, candidate) <= maxW {
			line = candidate
			continue
		}
		flush()
		if measure(face, word) <= maxW {
			line = word
			continue
		}
		// A single word wider than the panel: break it by runes.
		runes := []rune(word)
		cur := ""
		for _, r := range runes {
			if measure(face, cur+string(r)) > maxW && cur != "" {
				rows = append(rows, cur)
				cur = ""
			}
			cur += string(r)
		}
		line = cur
	}
	flush()
	if len(rows) == 0 {
		rows = []string{s}
	}
	return rows
}

// clipTo returns the sub-image a drawer may write into, so a scrolling line
// cannot paint over the header or the footer.
func clipTo(dst *image.RGBA, x, y, w, h float64) *image.RGBA {
	r := image.Rect(int(x), int(y), int(math.Ceil(x+w)), int(math.Ceil(y+h))).
		Intersect(dst.Bounds())
	if r.Empty() {
		return nil
	}
	sub, ok := dst.SubImage(r).(*image.RGBA)
	if !ok {
		return dst
	}
	return sub
}
