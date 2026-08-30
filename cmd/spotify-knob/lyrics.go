package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"spotify-knob/internal/controller"
	"spotify-knob/internal/lyrics"
	"spotify-knob/internal/openurl"
	"spotify-knob/internal/osd"
)

// lyricsManager connects three things that do not know about each other: the
// controller, which knows what is playing; the provider, which knows the
// words; and the panel, which draws them.
//
// It also owns the one piece of judgement in the feature. A press should
// either open a panel with lyrics in it or say "this track has no lyrics" -
// never open an empty panel and then close it. So a press waits briefly for
// the lookup, and only falls back to opening in a loading state when the
// answer is slow enough that waiting would feel broken.
type lyricsManager struct {
	ctl   *controller.Controller
	panel lyricsPanel
	card  noticeCard
	prov  lyricsSource
	log   *slog.Logger

	mu      sync.Mutex
	lastKey string
	opening bool
	openGen uint64
}

// lyricsPanel is the part of the window the manager uses, kept as an
// interface so the manager can be exercised without a real window.
type lyricsPanel interface {
	Visible() bool
	Show()
	Hide()
	SetTrack(osd.LyricsTrack)
	Loading(key string)
	Ready(key string, doc *osd.LyricDoc)
	Missing(key string)
	Failed(key string)
}

// lyricsSource is the lookup half of the provider. Narrowing it to these two
// calls is what lets the open-or-decline decision be tested without a network.
type lyricsSource interface {
	Get(ctx context.Context, q lyrics.Query) (*lyrics.Lyrics, error)
	Peek(q lyrics.Query) (*lyrics.Lyrics, bool)
}

// noticeCard is the overlay card, used for the small "no lyrics" message.
type noticeCard interface {
	ShowNotice(label, message string, t osd.Track)
}

// openWait is how long a press waits for a lookup before giving up and
// opening the panel in its loading state. A cached hit answers in
// microseconds and a cold one usually inside 300ms, so most presses either
// open with words already in place or never open at all.
const openWait = 450 * time.Millisecond

// followFreq is how often the panel is told where the playhead is. The panel
// extrapolates between updates, so this only has to be often enough to catch
// a track change and a seek.
const followFreq = 300 * time.Millisecond

// lyricsPollFreq is how often the panel forces a fresh read from Spotify
// while it is open, instead of waiting on the controller's own background
// poll (resync_seconds, 10s by default - tuned for keeping the volume from
// drifting, not for catching a skip made from Spotify's own app the instant
// it happens). A skip made anywhere other than the knob would otherwise take
// up to a full resync interval to show up here; this is what keeps an open
// panel closer to real time without polling any faster once it is closed.
const lyricsPollFreq = 2 * time.Second

func newLyricsManager(ctl *controller.Controller, panel lyricsPanel, card noticeCard,
	prov lyricsSource, log *slog.Logger) *lyricsManager {
	return &lyricsManager{ctl: ctl, panel: panel, card: card, prov: prov, log: log}
}

// Toggle is the knob gesture: open the panel, or close it if it is open.
func (m *lyricsManager) Toggle(ctx context.Context) {
	if m.panel.Visible() {
		m.panel.Hide()
		return
	}
	m.toggleFor(ctx, m.currentTrack(ctx))
}

// currentTrack is what is playing, forcing one poll if the daemon has not
// managed one yet. Without this, a press in the first seconds after boot
// answers "nothing is playing" while Spotify is plainly playing - the poll
// simply had not landed.
func (m *lyricsManager) currentTrack(ctx context.Context) controller.NowPlaying {
	np := m.ctl.Current()
	if np.Title != "" {
		return np
	}
	if err := m.ctl.Sync(ctx); err != nil {
		m.log.Debug("lyrics: could not read the player", "err", err)
		return np
	}
	return m.ctl.Current()
}

// beginOpen claims the right to open the panel and returns the generation
// that claim belongs to.
//
// A press that lands while an earlier one is still waiting on the network
// cancels it rather than stacking a second open behind it, so two quick
// presses read as open-then-changed-my-mind instead of open-then-open. The
// generation is what the in-flight lookup checks before it puts anything up.
func (m *lyricsManager) beginOpen() (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openGen++
	if m.opening {
		m.opening = false
		return 0, false
	}
	m.opening = true
	return m.openGen, true
}

func (m *lyricsManager) finishOpen(gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.openGen == gen {
		m.opening = false
	}
}

// openWanted reports whether the press that started this lookup still stands.
func (m *lyricsManager) openWanted(gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openGen == gen
}

// toggleFor is the opening half, split out from the controller so the
// open-or-decline decision can be tested on its own.
func (m *lyricsManager) toggleFor(ctx context.Context, np controller.NowPlaying) {
	gen, ok := m.beginOpen()
	if !ok {
		return // a second press cancelled the open that was still deciding
	}
	defer m.finishOpen(gen)

	if np.Title == "" {
		m.card.ShowNotice("LYRICS", "Nothing is playing", osd.Track{})
		return
	}
	q := queryFor(np)

	// A cached answer settles it with no wait at all, including a cached
	// "there are none".
	if doc, ok := m.prov.Peek(q); ok {
		if doc == nil {
			m.noLyrics(np)
			return
		}
		m.panel.SetTrack(lyricsTrack(np))
		m.panel.Ready(np.URI, convertDoc(doc))
		m.panel.Show()
		return
	}

	m.panel.SetTrack(lyricsTrack(np))
	m.panel.Loading(np.URI)

	type result struct {
		doc *lyrics.Lyrics
		err error
	}
	done := make(chan result, 1)
	go func() {
		doc, err := m.prov.Get(ctx, q)
		done <- result{doc, err}
	}()

	select {
	case r := <-done:
		if !m.openWanted(gen) {
			return
		}
		m.deliver(np, r.doc, r.err, true)
	case <-time.After(openWait):
		// Slow lookup: show the panel now so the press feels answered, and
		// fill it in when the words arrive.
		if !m.openWanted(gen) {
			return
		}
		m.panel.Show()
		go func() {
			r := <-done
			m.deliver(np, r.doc, r.err, false)
		}()
	case <-ctx.Done():
	}
}

// deliver puts a lookup's outcome on screen.
//
// deciding marks the one delivery that is still allowed to change whether the
// panel is up: the press's own answer, before anything has been shown. It is
// the same delivery that may decline to open at all and reply with the small
// card instead.
//
// Every other delivery - the fill-in behind a slow lookup, the follow along a
// track change - may only write into a panel that is already open, and is
// dropped if the user has closed it in the meantime. A window that reopens
// itself after being dismissed is the same bug the overlay card had, and it
// is worse than a missing update.
func (m *lyricsManager) deliver(np controller.NowPlaying, doc *lyrics.Lyrics, err error, deciding bool) {
	if !deciding && !m.panel.Visible() {
		return
	}
	switch {
	case err == nil && doc != nil:
		m.panel.Ready(np.URI, convertDoc(doc))
		if deciding {
			m.panel.Show()
		}
	case errors.Is(err, lyrics.ErrNotFound):
		if deciding {
			m.noLyrics(np)
			return
		}
		m.panel.Missing(np.URI)
	case errors.Is(err, context.Canceled):
		return
	default:
		m.log.Warn("lyrics lookup failed", "track", np.Title, "err", err)
		if deciding {
			m.card.ShowNotice("LYRICS", "Could not reach the lyrics service", osdTrack(np))
			return
		}
		m.panel.Failed(np.URI)
	}
}

func (m *lyricsManager) noLyrics(np controller.NowPlaying) {
	m.log.Debug("no lyrics", "track", np.Title, "artist", np.Artist)
	m.card.ShowNotice("LYRICS", "No lyrics for this track", osdTrack(np))
}

// run keeps an open panel pointed at whatever is playing.
func (m *lyricsManager) run(ctx context.Context) {
	tick := time.NewTicker(followFreq)
	defer tick.Stop()
	var lastPoll time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !m.panel.Visible() {
			continue
		}

		if time.Since(lastPoll) >= lyricsPollFreq {
			lastPoll = time.Now()
			if err := m.ctl.Sync(ctx); err != nil {
				m.log.Debug("lyrics: could not refresh the player", "err", err)
			}
		}

		np := m.ctl.Current()
		m.panel.SetTrack(lyricsTrack(np))
		if np.Title == "" {
			continue
		}

		key := np.URI + np.Title
		m.mu.Lock()
		changed := key != m.lastKey
		m.lastKey = key
		m.mu.Unlock()
		if !changed {
			continue
		}

		// The song moved on under an open panel. Unlike a press, this never
		// closes the panel: the user asked for lyrics to stay up, so a track
		// without them says so in place.
		m.panel.Loading(np.URI)
		go func(np controller.NowPlaying) {
			doc, err := m.prov.Get(ctx, queryFor(np))
			m.deliver(np, doc, err, false)
		}(np)
	}
}

func queryFor(np controller.NowPlaying) lyrics.Query {
	return lyrics.Query{
		Title:    np.Title,
		Artist:   np.Artist,
		Album:    np.Album,
		Duration: np.Duration,
		Key:      np.URI,
	}
}

func lyricsTrack(np controller.NowPlaying) osd.LyricsTrack {
	return osd.LyricsTrack{
		Title:      np.Title,
		Artist:     np.Artist,
		URI:        np.URI,
		ArtURL:     np.ArtURL,
		Duration:   np.Duration,
		Position:   np.Position,
		PositionAt: np.PositionAt,
		Playing:    np.Playing,
	}
}

func convertDoc(l *lyrics.Lyrics) *osd.LyricDoc {
	if l == nil {
		return nil
	}
	doc := &osd.LyricDoc{
		Synced:       l.Synced,
		Instrumental: l.Instrumental,
		Source:       l.Source,
		Lines:        make([]osd.LyricLine, 0, len(l.Lines)),
	}
	for _, ln := range l.Lines {
		doc.Lines = append(doc.Lines, osd.LyricLine{At: ln.At, Text: ln.Text})
	}
	return doc
}

// ---------------------------------------------------------------------------
// Where the panel was left

// panelState is what the user set by hand: where they dragged the panel, how
// big they made it, and how see-through they wanted it.
//
// It is deliberately not in config.json. Those values are set by dragging, and
// writing them back into a file the user also edits would fight the hot
// reload - every drag would look like a config change. An explicit edit to
// config.json still wins, because that is someone stating an intent rather
// than moving a window.
type panelState struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Opacity float64 `json:"opacity,omitempty"`
}

// panelStore keeps the last known state so a geometry change and an opacity
// change do not overwrite each other's half of the file.
type panelStore struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	state panelState
}

func newPanelStore(dir string, log *slog.Logger) *panelStore {
	st := &panelStore{path: filepath.Join(dir, "lyrics-window.json"), log: log}
	if b, err := os.ReadFile(st.path); err == nil {
		if json.Unmarshal(b, &st.state) != nil {
			st.state = panelState{}
		}
	}
	return st
}

func (s *panelStore) load() panelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Geometry records a finished move or resize.
func (s *panelStore) Geometry(x, y, w, h int) {
	s.mu.Lock()
	s.state.X, s.state.Y, s.state.W, s.state.H = x, y, w, h
	s.mu.Unlock()
	s.write()
}

// Opacity records where the transparency slider was let go.
func (s *panelStore) Opacity(v float64) {
	s.mu.Lock()
	s.state.Opacity = v
	s.mu.Unlock()
	s.write()
}

// write persists the state. It is called when a drag ends, so a failure is
// worth a debug line and nothing more.
func (s *panelStore) write() {
	s.mu.Lock()
	b, err := json.Marshal(s.state)
	s.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.WriteFile(s.path, b, 0o644); err != nil {
		s.log.Debug("could not remember the lyrics panel state", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Open in Spotify

// spotifyLaunchTargets orders what to try for the "Open in Spotify" button.
// The app itself goes first, since it can take you straight to the track
// inside whatever context you already had open; the web player is the
// fallback for a machine where the desktop app is not installed or has not
// registered the spotify: protocol.
func spotifyLaunchTargets(uri string) []string {
	targets := []string{uri}
	if web := spotifyWebFallback(uri); web != "" {
		targets = append(targets, web)
	}
	return targets
}

// spotifyWebFallback turns "spotify:track:ID" into the equivalent
// open.spotify.com page, or "" if uri is not that shape.
func spotifyWebFallback(uri string) string {
	const prefix = "spotify:"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	kind, id, ok := strings.Cut(strings.TrimPrefix(uri, prefix), ":")
	if !ok || kind == "" || id == "" {
		return ""
	}
	switch kind {
	case "track", "album", "artist", "playlist", "show", "episode":
		return "https://open.spotify.com/" + kind + "/" + id
	default:
		return ""
	}
}

// openInSpotify tries each launch target in order and stops at the first
// that succeeds, logging the rest as they fail. It is meant to run on its
// own goroutine - a broken browser association must not stall the panel.
func openInSpotify(uri string, log *slog.Logger) {
	if uri == "" {
		return
	}
	for _, target := range spotifyLaunchTargets(uri) {
		if err := openurl.Open(target); err == nil {
			return
		} else {
			log.Debug("could not open lyrics track target", "target", target, "err", err)
		}
	}
	log.Warn("could not open track in Spotify", "uri", uri)
}
