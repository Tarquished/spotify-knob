// Package controller holds the volume state machine that sits between the
// knob and the Spotify Web API.
//
// The knob can emit 10+ events per second. The API only offers an absolute
// PUT /me/player/volume, so every event would otherwise be one call and we
// would hit the rate limiter immediately.
//
// The scheduling is leading-edge: the first turn after a quiet moment goes out
// immediately, so a single click feels instant. Turns that land while a call is
// in flight are merged into the target and sent as one follow-up once the
// coalescing window has passed. At most one request is ever in flight.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"spotify-knob/internal/spotify"
)

// unknown marks "we have not read the real volume from Spotify yet".
const unknown = -1

// artMinPx is the smallest cover we are willing to download, so the OSD has
// pixels to spare when the display is scaled.
const artMinPx = 160

// queueDepth is how far ahead the lookahead reaches. Deeper than the peek
// card shows, because a burst of presses consumes one entry each and running
// out mid-burst is what makes a card name the wrong track.
const queueDepth = 10

// peekRows is how much of the lookahead the on-screen card displays.
const peekRows = 4

// NowPlaying is what the on-screen card shows about a track.
//
// Position is a snapshot taken at PositionAt: the card extrapolates from
// those two rather than asking Spotify where the playhead is 144 times a
// second.
type NowPlaying struct {
	Title      string
	Artist     string
	Album      string
	ArtURL     string
	URI        string
	Duration   time.Duration
	Position   time.Duration
	PositionAt time.Time
	Playing    bool
}

func (n NowPlaying) key() string { return n.Title + "␟" + n.Artist }

// atStart returns the track as it will look the instant it begins.
//
// Predictions come from the queue and from history, and a history entry
// carries the playhead from when that track was last seen. Showing it as-is
// would draw the progress line partway through a track that is about to
// restart from zero.
func (n NowPlaying) atStart() NowPlaying {
	n.Position = 0
	n.PositionAt = time.Now()
	n.Playing = true
	return n
}

// withoutTiming strips the playhead, for entries that are only kept as a
// record of what played.
func (n NowPlaying) withoutTiming() NowPlaying {
	n.Position = 0
	n.PositionAt = time.Time{}
	n.Playing = false
	return n
}

// Notifier receives things worth putting on screen. The OSD implements it;
// a nil Notifier simply means no overlay.
type Notifier interface {
	VolumeChanged(volume int, np NowPlaying)
	// TrackChanged is a skip the user just asked for; it puts a card up.
	TrackChanged(forward bool, np NowPlaying, pending bool)
	// TrackConfirmed is the watcher reporting what actually started playing.
	// It corrects a card that is still on screen and does nothing otherwise:
	// bringing a finished card back to announce a track as "next" when it is
	// already playing is worse than saying nothing.
	TrackConfirmed(forward bool, np NowPlaying)
	QueueChanged(queue []NowPlaying)
}

// writeSettle is how long after our own write we ignore the periodic resync,
// so a stale GET cannot undo the volume we just set.
const writeSettle = 3 * time.Second

// seekSettle is the same idea for the playhead. A poll already in flight when
// the user drags the progress rail would report the position from before the
// seek, and letting that win makes the rail visibly snap back.
const seekSettle = 2500 * time.Millisecond

type Controller struct {
	client *spotify.Client
	log    *slog.Logger

	step       int
	debounce   time.Duration
	resyncFreq time.Duration
	trackGuard time.Duration

	mu           sync.Mutex
	target       int  // volume we want Spotify to be at
	applied      int  // volume we last successfully pushed
	flushPending bool // a flush is scheduled or running
	lastFlush    time.Time
	lastWrite    time.Time
	lastSeek     time.Time
	lastTrack    time.Time
	backoff      time.Time // set from a 429 Retry-After
	device       string
	supports     bool
	playing      bool
	track        string
	lastErr      string
	lastErrAt    time.Time
	np           NowPlaying
	trackGen     uint64
	history      *trackHistory
	queue        []NowPlaying
	contextURI   string

	notify Notifier

	// trackSeq serialises skip requests; skips counts the ones still in
	// flight, so the watcher knows when the burst is over.
	trackSeq sync.Mutex
	skips    atomic.Int64
}

// SetNotifier attaches the on-screen display. Call before Run.
func (c *Controller) SetNotifier(n Notifier) {
	c.mu.Lock()
	c.notify = n
	c.mu.Unlock()
}

// Current is what is playing right now, as of the last poll. Position is a
// snapshot taken at PositionAt; callers that need the live playhead
// extrapolate from the pair rather than asking Spotify again.
func (c *Controller) Current() NowPlaying {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.np
}

func (c *Controller) notifier() (Notifier, NowPlaying) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notify, c.np
}

type Options struct {
	Step       int
	Debounce   time.Duration
	Resync     time.Duration
	TrackGuard time.Duration
}

func New(c *spotify.Client, log *slog.Logger, o Options) *Controller {
	return &Controller{
		client:     c,
		log:        log,
		step:       o.Step,
		debounce:   o.Debounce,
		resyncFreq: o.Resync,
		trackGuard: o.TrackGuard,
		target:     unknown,
		applied:    unknown,
		supports:   true,
		history:    newTrackHistory(24),
	}
}

// Run keeps the cached volume in sync with Spotify until ctx is cancelled.
// The periodic GET doubles as a keep-alive: it holds the TLS connection to
// api.spotify.com open, which takes ~50ms off every knob turn.
func (c *Controller) Run(ctx context.Context) {
	if err := c.Sync(ctx); err != nil {
		c.log.Info("initial sync skipped", "reason", err)
	} else {
		c.refreshQueue(ctx)
	}
	interval := c.resyncInterval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if now := c.resyncInterval(); now != interval {
				interval = now
				t.Reset(interval)
			}
			if c.syncDue() {
				if err := c.Sync(ctx); err != nil {
					c.log.Debug("resync failed", "err", err)
					continue
				}
				c.refreshQueue(ctx)
			}
		}
	}
}

// syncDue reports whether it is safe to overwrite local state from the API.
func (c *Controller) syncDue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.flushPending && time.Since(c.lastWrite) > writeSettle
}

// Sync reads the real volume back from Spotify so manual changes in the app
// do not leave the cached value drifting.
func (c *Controller) Sync(ctx context.Context) error {
	_, err := c.syncNow(ctx)
	return err
}

// syncNow is Sync, but hands the caller the player state it just read so a
// second round-trip is not needed to learn the track.
func (c *Controller) syncNow(ctx context.Context) (*spotify.PlayerState, error) {
	st, err := c.client.Player(ctx)
	if err != nil {
		c.noteErr(err)
		if errors.Is(err, spotify.ErrNoActiveDevice) {
			c.mu.Lock()
			c.target, c.applied, c.device = unknown, unknown, ""
			c.mu.Unlock()
		}
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.device = st.Device.Name
	c.supports = st.Device.SupportsVolume
	c.playing = st.IsPlaying
	c.track = st.Track()
	prev := c.np
	c.np = NowPlaying{
		Title:      st.Title(),
		Artist:     st.Artist(),
		Album:      st.Item.AlbumName(),
		ArtURL:     st.ArtURL(artMinPx),
		Duration:   st.Item.Duration(),
		Position:   st.Position(),
		PositionAt: time.Now(),
		Playing:    st.IsPlaying,
	}
	// A poll that was already in flight when the user seeked reports where the
	// playhead used to be. Keep ours until the write has had time to land,
	// otherwise the rail jumps back under the cursor.
	if !c.lastSeek.IsZero() && time.Since(c.lastSeek) < seekSettle &&
		prev.Title == c.np.Title {
		c.np.Position = prev.Position
		c.np.PositionAt = prev.PositionAt
	}
	if st.Item != nil {
		c.np.URI = st.Item.URI
	}
	c.contextURI = st.ContextURI()
	c.history.observe(c.np.withoutTiming())
	c.lastErr = ""
	if v := st.Device.VolumePercent; v != nil {
		if !c.flushPending && time.Since(c.lastWrite) > writeSettle {
			if c.target != *v {
				c.log.Debug("resynced volume", "from", c.target, "to", *v, "device", c.device)
			}
			c.target, c.applied = *v, *v
		}
	}
	return st, nil
}

// refreshQueue reads what Spotify will play next. Keeping this warm is what
// lets a knob press name the next track on the spot instead of showing a
// placeholder until the skip lands.
func (c *Controller) refreshQueue(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	q, err := c.client.Queue(qctx)
	if err != nil {
		c.log.Debug("queue lookup failed", "err", err)
		return
	}

	list := make([]NowPlaying, 0, queueDepth)
	for i := range q.Queue {
		if i >= queueDepth {
			break
		}
		it := &q.Queue[i]
		if it.Name == "" {
			continue
		}
		list = append(list, NowPlaying{
			Title:    it.Title(),
			Artist:   it.Artist(),
			Album:    it.AlbumName(),
			ArtURL:   it.ArtURL(artMinPx),
			URI:      it.URI,
			Duration: it.Duration(),
		})
	}

	c.mu.Lock()
	changed := !sameQueue(c.queue, list)
	c.queue = list
	visible := c.visibleQueueLocked()
	notify := c.notify
	c.mu.Unlock()

	if changed {
		c.log.Debug("queue lookahead", "depth", len(list))
		if notify != nil {
			notify.QueueChanged(visible)
		}
	}
}

// visibleQueueLocked is the slice the card shows. c.mu must be held.
func (c *Controller) visibleQueueLocked() []NowPlaying {
	n := len(c.queue)
	if n > peekRows {
		n = peekRows
	}
	return append([]NowPlaying(nil), c.queue[:n]...)
}

func sameQueue(a, b []NowPlaying) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].URI != b[i].URI || a[i].Title != b[i].Title {
			return false
		}
	}
	return true
}

// advanceQueue drops the tracks a skip has just consumed.
//
// Without this, two quick presses both predict from the same stale lookahead
// and the second card names the track that is already playing. The next
// refresh corrects whatever this gets wrong, but by then the cards have
// already been shown.
func (c *Controller) advanceQueue(n int) {
	c.mu.Lock()
	if n > len(c.queue) {
		n = len(c.queue)
	}
	c.queue = append([]NowPlaying(nil), c.queue[n:]...)
	visible := c.visibleQueueLocked()
	notify := c.notify
	c.mu.Unlock()

	if notify != nil {
		notify.QueueChanged(visible)
	}
}

// takeNext claims the next track and consumes it from the lookahead in one
// step. Predicting and consuming as separate operations let two presses in
// the same instant both claim the same track, which is exactly how a spammed
// knob ended up naming a song that was already playing.
func (c *Controller) takeNext() (NowPlaying, bool) {
	c.mu.Lock()
	if len(c.queue) == 0 || c.queue[0].Title == "" {
		c.mu.Unlock()
		return NowPlaying{}, false
	}
	np := c.queue[0].atStart()
	c.queue = append([]NowPlaying(nil), c.queue[1:]...)
	c.history.observe(np.withoutTiming())
	visible := c.visibleQueueLocked()
	notify := c.notify
	c.mu.Unlock()

	if notify != nil {
		notify.QueueChanged(visible)
	}
	return np, true
}

// takePrevious is the mirror image: step the history cursor back and put the
// track being left at the head of the lookahead.
func (c *Controller) takePrevious(leaving NowPlaying) (NowPlaying, bool) {
	c.mu.Lock()
	np, ok := c.history.previous()
	if !ok {
		c.mu.Unlock()
		return NowPlaying{}, false
	}
	np = np.atStart()
	c.history.observe(np.withoutTiming())
	if leaving.Title != "" {
		c.queue = append([]NowPlaying{leaving.withoutTiming()}, c.queue...)
		if len(c.queue) > queueDepth {
			c.queue = c.queue[:queueDepth]
		}
	}
	visible := c.visibleQueueLocked()
	notify := c.notify
	c.mu.Unlock()

	if notify != nil {
		notify.QueueChanged(visible)
	}
	return np, true
}

// Queue returns the upcoming tracks currently known.
func (c *Controller) Queue() []NowPlaying {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]NowPlaying(nil), c.queue...)
}

// PlayQueued jumps straight to a track from the lookahead.
func (c *Controller) PlayQueued(ctx context.Context, index int) {
	c.mu.Lock()
	if index < 0 || index >= len(c.queue) {
		c.mu.Unlock()
		return
	}
	target := c.queue[index]
	contextURI := c.contextURI
	c.trackGen++
	gen := c.trackGen
	notify := c.notify
	c.mu.Unlock()

	// The card can say what is coming before the request even goes out; we
	// picked the track, so there is nothing to guess.
	if notify != nil {
		notify.TrackChanged(true, target, false)
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()

	c.advanceQueue(index + 1)

	if index == 0 {
		// The very next track needs no context juggling.
		if err := c.client.Next(pctx); err != nil {
			c.noteErr(err)
			c.log.Error("play queued failed", "err", err)
			return
		}
	} else if err := c.client.Play(pctx, contextURI, target.URI); err != nil {
		c.noteErr(err)
		c.log.Error("play queued failed", "index", index, "err", err)
		return
	}
	c.log.Info("play queued", "index", index, "track", target.Title)

	go c.awaitTrack(context.WithoutCancel(ctx), gen, true, "")
}

// Adjust moves the target volume by delta steps (+1 / -1). It returns as soon
// as the state is updated, so the keyboard hook is never blocked on the
// network.
func (c *Controller) Adjust(ctx context.Context, delta int) {
	c.mu.Lock()
	if c.target == unknown {
		c.mu.Unlock()
		// First turn after startup, or after Spotify went idle: we need a base
		// value before we can do relative steps.
		if err := c.Sync(ctx); err != nil {
			c.log.Warn("cannot adjust volume", "err", err)
			return
		}
		c.mu.Lock()
		if c.target == unknown {
			c.mu.Unlock()
			return
		}
	}

	c.target = clamp(c.target + delta*c.step)
	target := c.target
	np := c.np
	notify := c.notify
	// The card is driven by the local target, not the API response, so it
	// appears the instant the knob moves.
	if notify != nil {
		defer notify.VolumeChanged(target, np)
	}
	if c.flushPending {
		// A flush is already scheduled or running; it will pick up this value.
		c.mu.Unlock()
		c.log.Debug("volume target", "target", target, "delta", delta, "coalesced", true)
		return
	}
	c.flushPending = true
	wait := c.debounce - time.Since(c.lastFlush)
	c.mu.Unlock()

	c.log.Debug("volume target", "target", target, "delta", delta, "wait", wait)
	c.schedule(context.WithoutCancel(ctx), wait)
}

// schedule runs a flush after wait, or immediately when the coalescing window
// has already elapsed.
func (c *Controller) schedule(ctx context.Context, wait time.Duration) {
	if wait <= 0 {
		go c.flush(ctx)
		return
	}
	time.AfterFunc(wait, func() { c.flush(ctx) })
}

// flush pushes the coalesced target to the API. Exactly one runs at a time.
func (c *Controller) flush(ctx context.Context) {
	c.mu.Lock()
	c.lastFlush = time.Now()
	target, applied, supports := c.target, c.applied, c.supports
	backoff := time.Until(c.backoff)
	c.mu.Unlock()

	if backoff > 0 {
		// Still inside a 429 backoff: wait it out rather than hammering.
		c.schedule(ctx, backoff)
		return
	}
	if target == unknown || target == applied {
		c.clearPending()
		return
	}
	if !supports {
		c.log.Warn("device does not support volume control, ignoring knob")
		c.clearPending()
		return
	}

	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	err := c.client.SetVolume(rctx, target)
	cancel()

	if err != nil {
		c.noteErr(err)
		var rl *spotify.RateLimitError
		switch {
		case errors.As(err, &rl):
			c.mu.Lock()
			c.backoff = time.Now().Add(rl.RetryAfter)
			c.mu.Unlock()
			c.log.Warn("rate limited by Spotify", "retry_after", rl.RetryAfter)
			c.schedule(ctx, rl.RetryAfter) // keeps flushPending set
		case errors.Is(err, spotify.ErrNoActiveDevice):
			c.mu.Lock()
			c.target, c.applied = unknown, unknown
			c.mu.Unlock()
			c.log.Info("no active Spotify device, start playback first")
			c.clearPending()
		case errors.Is(err, spotify.ErrVolumeUnsupported):
			c.mu.Lock()
			c.supports = false
			c.mu.Unlock()
			c.log.Warn("active device rejects volume control")
			c.clearPending()
		default:
			// Do not retry on our own: the next knob turn will try again.
			c.log.Error("set volume failed", "err", err, "target", target)
			c.clearPending()
		}
		return
	}

	c.mu.Lock()
	c.applied = target
	c.lastWrite = time.Now()
	c.lastErr = ""
	// Turns that landed while the request was in flight moved the target
	// again; send a follow-up once the coalescing window has passed.
	more := c.target != c.applied
	wait := c.debounce - time.Since(c.lastFlush)
	c.mu.Unlock()

	c.log.Info("volume set", "percent", target)
	if more {
		c.schedule(ctx, wait)
		return
	}
	c.clearPending()
}

func (c *Controller) clearPending() {
	c.mu.Lock()
	c.flushPending = false
	c.mu.Unlock()
}

// Next and Previous are guarded only lightly: a knob press is deliberate, we
// just refuse to forward machine-gun repeats.
// Seek moves the playhead of the current track.
//
// The local reading is updated before the request goes out, not after: the
// lyrics panel is drawing from it at up to 144fps, and waiting for a
// round-trip would show the rail snapping back to the old spot for the length
// of one API call.
func (c *Controller) Seek(ctx context.Context, pos time.Duration) {
	if pos < 0 {
		pos = 0
	}

	c.mu.Lock()
	if d := c.np.Duration; d > 0 && pos > d {
		pos = d
	}
	c.np.Position = pos
	c.np.PositionAt = time.Now()
	c.lastSeek = time.Now()
	np := c.np
	c.mu.Unlock()

	if err := c.client.Seek(ctx, pos); err != nil {
		c.log.Warn("seek failed", "position", pos, "err", err)
		c.noteErr(err)
		return
	}
	c.log.Info("seeked", "position", pos.Round(time.Second), "track", np.Title)
}

func (c *Controller) Next(ctx context.Context) { c.trackCmd(ctx, "next") }

func (c *Controller) Previous(ctx context.Context) { c.trackCmd(ctx, "previous") }

func (c *Controller) trackCmd(ctx context.Context, which string) {
	c.mu.Lock()
	if time.Since(c.lastTrack) < c.trackGuard || time.Now().Before(c.backoff) {
		c.mu.Unlock()
		c.log.Debug("track command dropped by guard", "cmd", which)
		return
	}
	c.lastTrack = time.Now()
	c.mu.Unlock()

	// Put the card up before the request goes out, not after it comes back.
	// We already know where the skip lands, so waiting for Spotify to confirm
	// only adds its round-trip to what the user perceives as key-to-pixel
	// latency. If the request then fails, awaitTrack has the last word.
	forward := which == "next"
	notify, before := c.notifier()

	// Claim the destination and consume it from the lookahead in one step.
	// Doing those separately let two presses in the same instant both claim
	// the same track, which is how a spammed knob named a song that was
	// already playing.
	var predicted NowPlaying
	var known bool
	if forward {
		predicted, known = c.takeNext()
	} else {
		predicted, known = c.takePrevious(before)
	}

	if notify != nil {
		if known {
			notify.TrackChanged(forward, predicted, false)
		} else {
			// Nothing known about where this lands. Keep the cover so the
			// card has something to show, but carry no title and no
			// duration: a progress line here would be the outgoing track's.
			notify.TrackChanged(forward, NowPlaying{ArtURL: before.ArtURL}, true)
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()

	// Skips go out one at a time. Fired concurrently, Spotify can apply them
	// in any order and no prediction can be right.
	c.skips.Add(1)
	c.trackSeq.Lock()
	var err error
	if which == "next" {
		err = c.client.Next(ctx)
	} else {
		err = c.client.Previous(ctx)
	}
	c.trackSeq.Unlock()
	c.skips.Add(-1)
	if err != nil {
		c.noteErr(err)
		var rl *spotify.RateLimitError
		if errors.As(err, &rl) {
			c.mu.Lock()
			c.backoff = time.Now().Add(rl.RetryAfter)
			c.mu.Unlock()
		}
		if errors.Is(err, spotify.ErrNoActiveDevice) {
			c.log.Info("no active Spotify device, start playback first")
			return
		}
		c.log.Error("track command failed", "cmd", which, "err", err)
		return
	}
	c.log.Info("track command", "cmd", which, "card", predicted.Title, "predicted", known)

	c.mu.Lock()
	c.trackGen++
	gen := c.trackGen
	c.mu.Unlock()

	go c.awaitTrack(context.WithoutCancel(ctx), gen, forward, before.key())
}

// Reconfigure applies settings that changed on disk, without a restart.
func (c *Controller) Reconfigure(o Options) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = o.Step
	c.debounce = o.Debounce
	c.trackGuard = o.TrackGuard
	c.resyncFreq = o.Resync
}

// resyncInterval is read by Run so a reconfigured interval takes effect.
func (c *Controller) resyncInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resyncFreq
}

// awaitTrack watches for the track Spotify actually switched to.
//
// The skip endpoint returns before the player reports the new track, so the
// card goes up with a placeholder and fills in here. Polling stops at the
// first change, and a newer skip supersedes an older watcher through the
// generation counter, so mashing the knob does not stack requests.
func (c *Controller) awaitTrack(ctx context.Context, gen uint64, forward bool, beforeKey string) {
	const attempts = 5

	// Wait for the burst to finish. Polling while skips are still being sent
	// latches onto whatever the player happens to be on midway through, which
	// is a real track but the wrong one.
	for i := 0; i < 40 && c.skips.Load() > 0; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
		if c.currentGen() != gen {
			return
		}
	}

	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(220 * time.Millisecond):
		}
		if c.currentGen() != gen {
			return
		}

		sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		st, err := c.syncNow(sctx)
		cancel()
		if err != nil || st == nil {
			continue
		}
		np := NowPlaying{Title: st.Title(), Artist: st.Artist(), ArtURL: st.ArtURL(artMinPx)}
		if np.Title == "" || np.key() == beforeKey {
			continue
		}
		if c.currentGen() != gen {
			return
		}
		if notify, _ := c.notifier(); notify != nil {
			notify.TrackConfirmed(forward, np)
		}
		// The lookahead is stale now; make the next press instant too.
		c.refreshQueue(ctx)
		return
	}

	// Never saw it change. Show what we know rather than leaving a
	// placeholder sitting on screen.
	if c.currentGen() != gen {
		return
	}
	if notify, np := c.notifier(); notify != nil && np.Title != "" {
		notify.TrackConfirmed(forward, np)
	}
}

func (c *Controller) currentGen() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.trackGen
}

func (c *Controller) noteErr(err error) {
	c.mu.Lock()
	c.lastErr = err.Error()
	c.lastErrAt = time.Now()
	c.mu.Unlock()
}

// Status is the snapshot served by GET /status.
type Status struct {
	Volume     int    `json:"volume"`
	Target     int    `json:"target"`
	Device     string `json:"device"`
	Supports   bool   `json:"supports_volume"`
	Playing    bool   `json:"playing"`
	Track      string `json:"track,omitempty"`
	Step       int    `json:"step"`
	DebounceMS int    `json:"debounce_ms"`
	LastError  string `json:"last_error,omitempty"`
	LastErrAgo string `json:"last_error_ago,omitempty"`
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Status{
		Volume:     c.applied,
		Target:     c.target,
		Device:     c.device,
		Supports:   c.supports,
		Playing:    c.playing,
		Track:      c.track,
		Step:       c.step,
		DebounceMS: int(c.debounce / time.Millisecond),
		LastError:  c.lastErr,
	}
	if c.lastErr != "" {
		s.LastErrAgo = time.Since(c.lastErrAt).Truncate(time.Second).String()
	}
	return s
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
