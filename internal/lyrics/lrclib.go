package lyrics

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means the lookup succeeded and the track simply has no lyrics.
// It is a normal outcome, not a failure, and the caller shows a small notice.
var ErrNotFound = errors.New("no lyrics for this track")

const (
	apiBase = "https://lrclib.net/api"

	// LRCLIB asks clients to identify themselves.
	userAgent = "spotify-knob/1.0 (https://github.com/spotify-knob)"

	// negativeTTL is how long a "no lyrics" answer is trusted. The database is
	// community-filled, so a miss today can be a hit next week; but refetching
	// on every press would hammer a free service for nothing.
	negativeTTL = 7 * 24 * time.Hour

	// durationSlack is how far a candidate's length may be from the track's
	// before it stops being the same recording. LRCLIB's own /get uses two
	// seconds; searching is allowed a little more because the metadata there
	// comes from many sources.
	durationSlack = 4 * time.Second
)

// Query identifies the track to look up.
type Query struct {
	Title    string
	Artist   string
	Album    string
	Duration time.Duration
	// Key uniquely identifies the track for caching; the Spotify URI when we
	// have one. Empty falls back to the metadata itself.
	Key string
}

func (q Query) cacheKey() string {
	k := q.Key
	if k == "" {
		k = strings.ToLower(q.Artist + "\x1f" + q.Title + "\x1f" + q.Duration.Round(time.Second).String())
	}
	sum := sha1.Sum([]byte(k))
	return hex.EncodeToString(sum[:])
}

func (q Query) valid() bool { return q.Title != "" }

// Provider looks lyrics up and remembers what it found.
//
// Two layers of cache: an in-memory map so reopening the window is instant,
// and a small JSON file per track so a daemon restart is instant too. Misses
// are cached as well, with a shorter life.
type Provider struct {
	http *http.Client
	dir  string
	log  *slog.Logger

	mu       sync.Mutex
	mem      map[string]*cached
	inFlight map[string]chan struct{}
}

type cached struct {
	Lyrics  *Lyrics   `json:"lyrics,omitempty"`
	Missing bool      `json:"missing,omitempty"`
	At      time.Time `json:"at"`
}

// New builds a provider. cacheDir may be empty, which disables the disk half.
func New(cacheDir string, log *slog.Logger) *Provider {
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			log.Warn("lyrics cache unavailable", "dir", cacheDir, "err", err)
			cacheDir = ""
		}
	}
	return &Provider{
		// Generous but bounded: this runs off the UI thread, and a hung
		// request must not pin the window in "Loading" forever.
		http:     &http.Client{Timeout: 12 * time.Second},
		dir:      cacheDir,
		log:      log,
		mem:      make(map[string]*cached),
		inFlight: make(map[string]chan struct{}),
	}
}

// Get returns the lyrics for a track, or ErrNotFound.
//
// Concurrent calls for the same track collapse onto one request: the window
// polls the playing track, so a track change can easily ask twice at once.
func (p *Provider) Get(ctx context.Context, q Query) (*Lyrics, error) {
	if !q.valid() {
		return nil, ErrNotFound
	}
	key := q.cacheKey()

	for {
		p.mu.Lock()
		if c := p.lookupLocked(key); c != nil {
			p.mu.Unlock()
			if c.Missing {
				return nil, ErrNotFound
			}
			return c.Lyrics, nil
		}
		wait, busy := p.inFlight[key]
		if !busy {
			wait = make(chan struct{})
			p.inFlight[key] = wait
			p.mu.Unlock()
			break
		}
		p.mu.Unlock()

		select {
		case <-wait:
			continue // the winner has filled the cache; read it
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	lyr, err := p.fetch(ctx, q)

	p.mu.Lock()
	if err == nil || errors.Is(err, ErrNotFound) {
		entry := &cached{Lyrics: lyr, Missing: err != nil, At: time.Now()}
		p.mem[key] = entry
		p.store(key, entry)
	}
	close(p.inFlight[key])
	delete(p.inFlight, key)
	p.mu.Unlock()

	return lyr, err
}

// Peek returns an already-known result without going to the network.
func (p *Provider) Peek(q Query) (*Lyrics, bool) {
	if !q.valid() {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.lookupLocked(q.cacheKey()); c != nil {
		return c.Lyrics, true
	}
	return nil, false
}

// lookupLocked checks memory then disk. Callers hold p.mu.
func (p *Provider) lookupLocked(key string) *cached {
	if c, ok := p.mem[key]; ok {
		if c.Missing && time.Since(c.At) > negativeTTL {
			delete(p.mem, key)
			return nil
		}
		return c
	}
	if p.dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(p.dir, key+".json"))
	if err != nil {
		return nil
	}
	var c cached
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	if c.Missing && time.Since(c.At) > negativeTTL {
		return nil
	}
	p.mem[key] = &c
	return &c
}

func (p *Provider) store(key string, c *cached) {
	if p.dir == "" {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := filepath.Join(p.dir, key+".json.tmp")
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, key+".json")); err != nil {
		os.Remove(tmp)
	}
}

// record is LRCLIB's response shape, shared by /get and /search.
type record struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

func (p *Provider) fetch(ctx context.Context, q Query) (*Lyrics, error) {
	// The exact endpoint first: it matches on duration, so a hit here is the
	// right recording rather than a same-titled cover.
	if rec, err := p.exact(ctx, q); err == nil {
		if l := convert(rec); l != nil {
			return l, nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	recs, err := p.search(ctx, q)
	if err != nil {
		return nil, err
	}
	best := pick(recs, q)
	if best == nil {
		return nil, ErrNotFound
	}
	l := convert(best)
	if l == nil {
		return nil, ErrNotFound
	}
	return l, nil
}

func (p *Provider) exact(ctx context.Context, q Query) (*record, error) {
	v := url.Values{}
	v.Set("artist_name", q.Artist)
	v.Set("track_name", q.Title)
	if q.Album != "" {
		v.Set("album_name", q.Album)
	}
	if q.Duration > 0 {
		v.Set("duration", fmt.Sprintf("%d", int(q.Duration.Round(time.Second)/time.Second)))
	}

	var rec record
	if err := p.getJSON(ctx, apiBase+"/get?"+v.Encode(), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (p *Provider) search(ctx context.Context, q Query) ([]record, error) {
	v := url.Values{}
	v.Set("track_name", q.Title)
	if q.Artist != "" {
		v.Set("artist_name", q.Artist)
	}

	var recs []record
	if err := p.getJSON(ctx, apiBase+"/search?"+v.Encode(), &recs); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(recs) > 0 {
		return recs, nil
	}

	// Nothing under this exact artist. Spotify credits every featured artist
	// in one string ("A, B, C") while LRCLIB usually files the track under the
	// lead only, so retry on the title alone rather than giving up.
	v = url.Values{}
	v.Set("track_name", q.Title)
	if err := p.getJSON(ctx, apiBase+"/search?"+v.Encode(), &recs); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, ErrNotFound
	}
	return recs, nil
}

func (p *Provider) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("lrclib: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// convert turns a record into displayable lyrics, preferring the synced form.
func convert(r *record) *Lyrics {
	if r == nil {
		return nil
	}
	l := &Lyrics{Title: r.TrackName, Artist: r.ArtistName, Source: "LRCLIB"}
	if r.Instrumental {
		l.Instrumental = true
		return l
	}
	if lines := parseLRC(r.SyncedLyrics); len(lines) > 0 {
		l.Lines, l.Synced = lines, true
		return l
	}
	if strings.TrimSpace(r.PlainLyrics) != "" {
		l.Lines = plainLines(r.PlainLyrics)
		return l
	}
	return nil
}

// pick scores search results against what is actually playing.
//
// Searching by title returns covers, live versions, and "- Topic" uploads of
// the same song. Duration is the strongest signal that two recordings are the
// same one, and synced lyrics are worth more than plain because the whole
// point of the view is the highlight.
func pick(recs []record, q Query) *record {
	want := q.Duration.Seconds()
	title := normalise(q.Title)
	artist := normalise(q.Artist)

	best, bestScore := -1, math.Inf(-1)
	for i := range recs {
		r := &recs[i]
		if r.SyncedLyrics == "" && strings.TrimSpace(r.PlainLyrics) == "" && !r.Instrumental {
			continue
		}
		score := 0.0

		switch rt := normalise(r.TrackName); {
		case rt == title:
			score += 6
		case strings.HasPrefix(rt, title) || strings.HasPrefix(title, rt):
			score += 3
		case strings.Contains(rt, title) || strings.Contains(title, rt):
			score += 1
		default:
			score -= 4
		}

		if ra := normalise(r.ArtistName); ra == artist {
			score += 4
		} else if artist != "" && (strings.Contains(artist, ra) || strings.Contains(ra, artist)) {
			score += 2
		}

		if want > 0 && r.Duration > 0 {
			diff := math.Abs(r.Duration - want)
			switch {
			case diff <= durationSlack.Seconds():
				score += 5 - diff // closer is better, continuously
			case diff <= 15:
				score += 1
			default:
				score -= 6 // a different length is a different recording
			}
		}

		if r.SyncedLyrics != "" {
			score += 3
		}

		if score > bestScore {
			best, bestScore = i, score
		}
	}
	// A negative best is a result that failed on both title and length; it is
	// more honest to say there are no lyrics than to show the wrong song's.
	if best < 0 || bestScore < 0 {
		return nil
	}
	return &recs[best]
}

// normalise strips the decoration that stops two spellings of one title from
// matching: case, punctuation, and the bracketed suffixes labels love.
func normalise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, cut := range []string{" - topic", " (remastered", " - remastered", " (feat.", " (with "} {
		if i := strings.Index(s, cut); i > 0 {
			s = s[:i]
		}
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}
