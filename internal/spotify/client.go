// Package spotify is a thin client over the handful of Web API endpoints this
// tool needs.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.spotify.com/v1"

var (
	// ErrNoActiveDevice means Spotify is not playing anywhere right now.
	ErrNoActiveDevice = errors.New("no active Spotify device")
	// ErrVolumeUnsupported means the active device rejects remote volume control.
	ErrVolumeUnsupported = errors.New("active device does not support volume control")
	// ErrPremiumRequired is returned for the 403 Spotify sends to free accounts.
	ErrPremiumRequired = errors.New("Spotify Premium required for playback control")
)

// RateLimitError carries the Retry-After hint from a 429.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// APIError is any other non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("spotify api %d: %s", e.Status, e.Message) }

// Device is the playback target reported by GET /me/player.
type Device struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	IsActive       bool   `json:"is_active"`
	VolumePercent  *int   `json:"volume_percent"`
	SupportsVolume bool   `json:"supports_volume"`
}

// Image is one size of album artwork.
type Image struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Item is a track, as returned by the player and queue endpoints.
type Item struct {
	Name       string `json:"name"`
	URI        string `json:"uri"`
	DurationMS int    `json:"duration_ms"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name   string  `json:"name"`
		Images []Image `json:"images"`
	} `json:"album"`
}

// Duration is how long the track runs.
func (i *Item) Duration() time.Duration {
	if i == nil {
		return 0
	}
	return time.Duration(i.DurationMS) * time.Millisecond
}

// Title is the track name.
func (i *Item) Title() string {
	if i == nil {
		return ""
	}
	return i.Name
}

// Artist joins the credited artists, which is what listeners expect to read.
func (i *Item) Artist() string {
	if i == nil {
		return ""
	}
	names := make([]string, 0, len(i.Artists))
	for _, a := range i.Artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// AlbumName is the release the track belongs to. Lyrics lookups use it to
// tell an album cut apart from the same song on a compilation.
func (i *Item) AlbumName() string {
	if i == nil {
		return ""
	}
	return i.Album.Name
}

// ArtURL picks the smallest cover at least minPx wide, falling back to the
// largest available. Downloading a 640px cover to draw it at 68px is waste.
func (i *Item) ArtURL(minPx int) string {
	if i == nil || len(i.Album.Images) == 0 {
		return ""
	}
	best, bestW := "", 1<<30
	largest, largestW := "", -1
	for _, im := range i.Album.Images {
		if im.URL == "" {
			continue
		}
		if im.Width > largestW {
			largestW, largest = im.Width, im.URL
		}
		if im.Width >= minPx && im.Width < bestW {
			bestW, best = im.Width, im.URL
		}
	}
	if best != "" {
		return best
	}
	return largest
}

// PlayerState is the subset of GET /me/player we care about.
type PlayerState struct {
	Device     Device `json:"device"`
	IsPlaying  bool   `json:"is_playing"`
	ProgressMS int    `json:"progress_ms"`
	Context    *struct {
		URI string `json:"uri"`
	} `json:"context"`
	Item *Item `json:"item"`
}

// Position is how far into the track playback has reached.
func (p *PlayerState) Position() time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(p.ProgressMS) * time.Millisecond
}

// ContextURI is the playlist or album the track is playing from, empty when
// Spotify is not playing from a context.
func (p *PlayerState) ContextURI() string {
	if p == nil || p.Context == nil {
		return ""
	}
	return p.Context.URI
}

func (p *PlayerState) Title() string {
	if p == nil {
		return ""
	}
	return p.Item.Title()
}

func (p *PlayerState) Artist() string {
	if p == nil {
		return ""
	}
	return p.Item.Artist()
}

func (p *PlayerState) ArtURL(minPx int) string {
	if p == nil {
		return ""
	}
	return p.Item.ArtURL(minPx)
}

// Track returns "Artist - Title" for logging and /status.
func (p *PlayerState) Track() string {
	if p == nil || p.Item == nil {
		return ""
	}
	if a := p.Artist(); a != "" {
		return a + " - " + p.Item.Name
	}
	return p.Item.Name
}

// QueueState is GET /me/player/queue: what is playing and what comes next.
type QueueState struct {
	CurrentlyPlaying *Item  `json:"currently_playing"`
	Queue            []Item `json:"queue"`
}

// Up returns the track that will play next, if Spotify knows one.
func (q *QueueState) Up() *Item {
	if q == nil || len(q.Queue) == 0 {
		return nil
	}
	return &q.Queue[0]
}

// TokenSource supplies bearer tokens. auth.Manager implements it; tests
// substitute a stub.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	Invalidate()
}

// Client talks to the Spotify Web API.
type Client struct {
	auth TokenSource
	base string
	hc   *http.Client
	log  *slog.Logger
}

func New(ts TokenSource, log *slog.Logger) *Client {
	return &Client{
		auth: ts,
		base: apiBase,
		hc:   &http.Client{Timeout: 10 * time.Second},
		log:  log,
	}
}

// SetBaseURL points the client at a different API root. Only used by tests.
func (c *Client) SetBaseURL(u string) { c.base = u }

// Player returns the current playback state, or ErrNoActiveDevice.
func (c *Client) Player(ctx context.Context) (*PlayerState, error) {
	body, err := c.do(ctx, http.MethodGet, "/me/player", nil)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		// 204 No Content: nothing is playing.
		return nil, ErrNoActiveDevice
	}
	var st PlayerState
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("decode player state: %w", err)
	}
	return &st, nil
}

// Queue reads the upcoming tracks. This is what lets a knob press name the
// next song immediately instead of waiting for the skip to take effect.
func (c *Client) Queue(ctx context.Context) (*QueueState, error) {
	body, err := c.do(ctx, http.MethodGet, "/me/player/queue", nil)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, ErrNoActiveDevice
	}
	var q QueueState
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, fmt.Errorf("decode queue: %w", err)
	}
	return &q, nil
}

// Play starts a specific track.
//
// Jumping to a track inside the queue has no dedicated endpoint. Playing it
// by URI alone works but throws away the playlist you were listening to, so
// when Spotify reports a context we ask for that context at an offset, which
// keeps everything after the chosen track intact. Without a context (a
// manually queued track, for instance) there is nothing to preserve and the
// bare URI is right.
func (c *Client) Play(ctx context.Context, contextURI, trackURI string) error {
	if trackURI == "" {
		return errors.New("no track to play")
	}
	if contextURI != "" {
		body := fmt.Sprintf(`{"context_uri":%q,"offset":{"uri":%q}}`, contextURI, trackURI)
		_, err := c.do(ctx, http.MethodPut, "/me/player/play", strings.NewReader(body))
		if err == nil {
			return nil
		}
		// The track may not belong to the context; fall through to the
		// context-free form rather than failing the gesture.
		c.log.Debug("contextual play failed, retrying by uri", "err", err)
	}
	body := fmt.Sprintf(`{"uris":[%q]}`, trackURI)
	_, err := c.do(ctx, http.MethodPut, "/me/player/play", strings.NewReader(body))
	return err
}

// SetVolume sets the absolute volume (0-100) on the active device.
func (c *Client) SetVolume(ctx context.Context, percent int) error {
	q := url.Values{"volume_percent": {strconv.Itoa(percent)}}
	_, err := c.do(ctx, http.MethodPut, "/me/player/volume?"+q.Encode(), nil)
	return err
}

// Seek moves the playhead. Spotify takes an absolute offset in
// milliseconds; there is no relative form.
func (c *Client) Seek(ctx context.Context, pos time.Duration) error {
	ms := int(pos / time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	q := url.Values{"position_ms": {strconv.Itoa(ms)}}
	_, err := c.do(ctx, http.MethodPut, "/me/player/seek?"+q.Encode(), nil)
	return err
}

// Next skips to the next track.
func (c *Client) Next(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/next", nil)
	return err
}

// Previous goes back a track.
func (c *Client) Previous(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/previous", nil)
	return err
}

// do performs a request, refreshing the token once on a 401 and mapping the
// documented failure modes onto typed errors.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	b, err := c.attempt(ctx, method, path, body)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		c.log.Debug("got 401, refreshing token and retrying once")
		c.auth.Invalidate()
		return c.attempt(ctx, method, path, body)
	}
	return b, err
}

func (c *Client) attempt(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	tok, err := c.auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	} else if method != http.MethodGet {
		// Spotify wants a zero Content-Length on these, not a chunked body.
		req.Header.Set("Content-Length", "0")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return raw, nil
	case resp.StatusCode == http.StatusNotFound:
		// 404 on the player endpoints means "no active device", not "bad URL".
		return nil, ErrNoActiveDevice
	case resp.StatusCode == http.StatusTooManyRequests:
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		if secs <= 0 {
			secs = 1
		}
		return nil, &RateLimitError{RetryAfter: time.Duration(secs) * time.Second}
	}

	msg := apiMessage(raw)
	if resp.StatusCode == http.StatusForbidden {
		switch {
		case containsAny(strings.ToLower(msg), "volume_control_disallow", "volume"):
			return nil, ErrVolumeUnsupported
		case containsAny(strings.ToLower(msg), "premium"):
			return nil, ErrPremiumRequired
		}
	}
	return nil, &APIError{Status: resp.StatusCode, Message: msg}
}

func apiMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && (e.Error.Message != "" || e.Error.Reason != "") {
		if e.Error.Reason != "" {
			return e.Error.Reason + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
