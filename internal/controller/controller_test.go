package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"spotify-knob/internal/spotify"
)

type stubToken struct{}

func (stubToken) AccessToken(context.Context) (string, error) { return "test-token", nil }
func (stubToken) Invalidate()                                 {}

// fakeAPI stands in for the Spotify Web API and records every volume write.
type fakeAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	volume   int
	playing  string
	progress int
	writes   []int
	nexts    int
	prevs    int
	offline  bool
	upNext   string
}

func newFakeAPI(t *testing.T, startVolume int) *fakeAPI {
	t.Helper()
	f := &fakeAPI{volume: startVolume, playing: "Current", upNext: "Up Next"}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /me/player", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.offline {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"item":        trackJSON(f.playing, "Current Artist"),
			"progress_ms": f.progress,
			"device": map[string]any{
				"name":            "TEST-PC",
				"type":            "Computer",
				"is_active":       true,
				"volume_percent":  f.volume,
				"supports_volume": true,
			},
			"is_playing": true,
		})
	})

	mux.HandleFunc("PUT /me/player/volume", func(w http.ResponseWriter, r *http.Request) {
		v, err := strconv.Atoi(r.URL.Query().Get("volume_percent"))
		if err != nil {
			http.Error(w, "bad volume", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.volume = v
		f.writes = append(f.writes, v)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /me/player/next", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nexts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /me/player/previous", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.prevs++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /me/player/queue", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"currently_playing": trackJSON("Current", "Someone"),
			"queue":             queueJSON(f.upNext),
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// queueJSON is a lookahead deep enough to survive a burst of presses.
func queueJSON(first string) []any {
	names := []string{first, "Second Up", "Third Up", "Q4", "Q5", "Q6", "Q7", "Q8", "Q9", "Q10"}
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, trackJSON(n, "Queue Artist"))
	}
	return out
}

func trackJSON(name, artist string) map[string]any {
	return map[string]any{
		"name":        name,
		"uri":         "spotify:track:" + name,
		"duration_ms": 210000,
		"artists":     []any{map[string]any{"name": artist}},
		"album": map[string]any{
			"images": []any{map[string]any{"url": "https://example.invalid/" + name, "width": 300}},
		},
	}
}

func (f *fakeAPI) snapshot() (writes []int, nexts, prevs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.writes...), f.nexts, f.prevs
}

func newTestController(t *testing.T, f *fakeAPI, step int, debounce time.Duration) *Controller {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := spotify.New(stubToken{}, log)
	c.SetBaseURL(f.srv.URL)
	return New(c, log, Options{
		Step:       step,
		Debounce:   debounce,
		Resync:     time.Hour,
		TrackGuard: 250 * time.Millisecond,
	})
}

// recorder captures what the on-screen card was told to show.
type recordingNotifier struct {
	mu       sync.Mutex
	tracks   []trackCall
	confirms []trackCall
	queue    []NowPlaying
}

type trackCall struct {
	forward bool
	np      NowPlaying
	pending bool
}

func (r *recordingNotifier) VolumeChanged(int, NowPlaying) {}

func (r *recordingNotifier) TrackConfirmed(forward bool, np NowPlaying) {
	r.mu.Lock()
	r.confirms = append(r.confirms, trackCall{forward, np, false})
	r.mu.Unlock()
}

func (r *recordingNotifier) confirmed() []trackCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]trackCall(nil), r.confirms...)
}

func (r *recordingNotifier) QueueChanged(q []NowPlaying) {
	r.mu.Lock()
	r.queue = append([]NowPlaying(nil), q...)
	r.mu.Unlock()
}

func (r *recordingNotifier) TrackChanged(forward bool, np NowPlaying, pending bool) {
	r.mu.Lock()
	r.tracks = append(r.tracks, trackCall{forward, np, pending})
	r.mu.Unlock()
}

func (r *recordingNotifier) calls() []trackCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]trackCall(nil), r.tracks...)
}

// Pressing the knob must name the next track straight away, using Spotify's
// own queue, instead of showing a placeholder until the skip takes effect.
func TestNextNamesTheTrackImmediately(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	ctl.Next(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) > 0 })

	first := rec.calls()[0]
	if first.pending {
		t.Fatal("the first card should already carry the track, not a placeholder")
	}
	if first.np.Title != "Up Next" {
		t.Fatalf("want the queued track, got %q", first.np.Title)
	}
	if !first.forward {
		t.Fatal("direction should be forward")
	}
	if first.np.ArtURL == "" {
		t.Fatal("predicted track should carry its cover so the card is complete")
	}
}

// With nothing known ahead, the card falls back to a placeholder rather than
// inventing a title.
func TestNextFallsBackToPendingWithoutALookahead(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)

	ctl.Next(context.Background())
	waitFor(t, time.Second, func() bool { return len(rec.calls()) > 0 })

	if !rec.calls()[0].pending {
		t.Fatal("without a lookahead the card must show as pending")
	}
}

// Previous is predicted from what we watched play, so it is instant too.
func TestPreviousNamesTheTrackFromHistory(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	f.mu.Lock()
	f.playing = "First"
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.playing = "Second"
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	ctl.Previous(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) > 0 })

	first := rec.calls()[0]
	if first.pending {
		t.Fatal("previous should be named from history, not left pending")
	}
	if first.np.Title != "First" {
		t.Fatalf("want First, got %q", first.np.Title)
	}
	if first.forward {
		t.Fatal("direction should be backward")
	}
}

// A burst of knob clicks must not become one API call per click. Leading edge
// means the first click goes out at once; everything after it is coalesced, so
// a six-click spin costs two calls at most and lands on the final value.
func TestAdjustCoalescesBurst(t *testing.T) {
	f := newFakeAPI(t, 40)
	ctl := newTestController(t, f, 5, 80*time.Millisecond)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		ctl.Adjust(ctx, +1)
	}

	waitFor(t, 2*time.Second, func() bool {
		w, _, _ := f.snapshot()
		return len(w) > 0 && w[len(w)-1] == 70
	})
	time.Sleep(250 * time.Millisecond) // let any stragglers land

	writes, _, _ := f.snapshot()
	if len(writes) > 2 {
		t.Fatalf("6-click burst should cost at most 2 API calls, got %d: %v", len(writes), writes)
	}
	if got := writes[len(writes)-1]; got != 70 {
		t.Fatalf("want final volume 70 (40 + 6*5), got %d (%v)", got, writes)
	}
}

// The first turn after a quiet moment must not wait out the debounce window.
// This is the difference between the knob feeling instant and feeling laggy.
func TestFirstAdjustFiresWithoutWaiting(t *testing.T) {
	f := newFakeAPI(t, 40)
	const debounce = 400 * time.Millisecond
	ctl := newTestController(t, f, 5, debounce)

	if err := ctl.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ctl.Adjust(context.Background(), +1)
	waitFor(t, 2*time.Second, func() bool {
		w, _, _ := f.snapshot()
		return len(w) == 1
	})
	elapsed := time.Since(start)

	if elapsed >= debounce {
		t.Fatalf("first turn waited %s, should fire well inside the %s window", elapsed, debounce)
	}
	writes, _, _ := f.snapshot()
	if writes[0] != 45 {
		t.Fatalf("want 45, got %d", writes[0])
	}
}

func TestAdjustClampsToRange(t *testing.T) {
	f := newFakeAPI(t, 95)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		ctl.Adjust(ctx, +1)
	}
	waitFor(t, 2*time.Second, func() bool {
		w, _, _ := f.snapshot()
		return len(w) > 0 && w[len(w)-1] == 100
	})

	for i := 0; i < 40; i++ {
		ctl.Adjust(ctx, -1)
	}
	waitFor(t, 2*time.Second, func() bool {
		w, _, _ := f.snapshot()
		return w[len(w)-1] == 0
	})
}

// Turning the knob while nothing is playing must not blow up.
func TestAdjustWithNoActiveDevice(t *testing.T) {
	f := newFakeAPI(t, 50)
	f.offline = true
	ctl := newTestController(t, f, 5, 50*time.Millisecond)

	ctl.Adjust(context.Background(), +1)
	time.Sleep(150 * time.Millisecond)

	writes, _, _ := f.snapshot()
	if len(writes) != 0 {
		t.Fatalf("no device active, expected no volume writes, got %v", writes)
	}
	if got := ctl.Status().Target; got != unknown {
		t.Fatalf("target should stay unknown, got %d", got)
	}
}

// Two presses in quick succession are one next and one previous, not four
// commands: the guard only drops machine-gun repeats.
func TestTrackGuardDropsRepeats(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctx := context.Background()

	ctl.Next(ctx)
	ctl.Next(ctx) // inside the guard window, dropped
	waitFor(t, time.Second, func() bool {
		_, n, _ := f.snapshot()
		return n >= 1
	})
	time.Sleep(100 * time.Millisecond)

	_, nexts, _ := f.snapshot()
	if nexts != 1 {
		t.Fatalf("want 1 next through the guard, got %d", nexts)
	}

	time.Sleep(300 * time.Millisecond) // guard expires
	ctl.Previous(ctx)
	waitFor(t, time.Second, func() bool {
		_, _, p := f.snapshot()
		return p == 1
	})
}

// Changing the volume inside the Spotify app must win once the write window
// has passed, so the cached value cannot drift.
func TestSyncPicksUpExternalVolumeChange(t *testing.T) {
	f := newFakeAPI(t, 30)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ctl.Status().Target; got != 30 {
		t.Fatalf("want target 30 after sync, got %d", got)
	}

	f.mu.Lock()
	f.volume = 80 // user dragged the slider in the app
	f.mu.Unlock()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ctl.Status().Target; got != 80 {
		t.Fatalf("want target 80 after resync, got %d", got)
	}

	ctl.Adjust(ctx, +1)
	waitFor(t, 2*time.Second, func() bool {
		w, _, _ := f.snapshot()
		return len(w) > 0 && w[len(w)-1] == 85
	})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// A track we are switching to always starts at zero. History entries carry
// the playhead from when they last played, and showing that would draw the
// progress line partway through a song that is about to restart.
func TestPredictedTrackStartsAtZero(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	// Play one track well into its runtime, then move on.
	f.mu.Lock()
	f.playing, f.progress = "First", 120000
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.playing, f.progress = "Second", 5000
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	ctl.Previous(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) > 0 })

	got := rec.calls()[0].np
	if got.Title != "First" {
		t.Fatalf("want First, got %q", got.Title)
	}
	if got.Position != 0 {
		t.Fatalf("a track being switched to must start at 0, got %s", got.Position)
	}
	if !got.Playing {
		t.Fatal("the predicted track should be marked as playing so the line advances")
	}
	if got.PositionAt.IsZero() {
		t.Fatal("PositionAt must be stamped, or the card cannot extrapolate")
	}
}

// Two quick presses must name two different tracks. Before the lookahead was
// advanced with the skip, the second press repeated the first prediction -
// which by then was the track already playing.
func TestBackToBackSkipsNameDifferentTracks(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	// Shrink the anti-spam guard so both presses get through; the point here
	// is what the two cards say, not the rate limiting.
	ctl.Reconfigure(Options{Step: 5, Debounce: 50 * time.Millisecond,
		Resync: time.Hour, TrackGuard: 5 * time.Millisecond})
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	ctl.Next(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) >= 1 })
	time.Sleep(20 * time.Millisecond)
	ctl.Next(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) >= 2 })

	calls := rec.calls()
	first, second := calls[0].np.Title, calls[1].np.Title
	if first == second {
		t.Fatalf("both skips named %q; the lookahead did not advance", first)
	}
	if first != "Up Next" || second != "Second Up" {
		t.Fatalf("want Up Next then Second Up, got %q then %q", first, second)
	}
}

// Going back should put the track we left at the head of the lookahead, so
// the peek and the next prediction agree with what actually happens.
func TestPreviousRewindsTheLookahead(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctl.Reconfigure(Options{Step: 5, Debounce: 50 * time.Millisecond,
		Resync: time.Hour, TrackGuard: 5 * time.Millisecond})
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	f.mu.Lock()
	f.playing = "First"
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.playing = "Second"
	f.mu.Unlock()
	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	ctl.Previous(ctx)
	waitFor(t, time.Second, func() bool { return len(rec.calls()) > 0 })

	q := ctl.Queue()
	if len(q) == 0 || q[0].Title != "Second" {
		t.Fatalf("the track we left should head the lookahead, got %v", titles(q))
	}
}

func titles(q []NowPlaying) []string {
	out := make([]string, len(q))
	for i, n := range q {
		out[i] = n.Title
	}
	return out
}

// Spamming the knob is the case that broke: five presses inside a second used
// to run the lookahead dry and start naming tracks that were already playing.
// Each press must name the next track along, in order, with no repeats.
func TestSpammedSkipsNameEachTrackInOrder(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctl.Reconfigure(Options{Step: 5, Debounce: 50 * time.Millisecond,
		Resync: time.Hour, TrackGuard: time.Millisecond})
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	const presses = 5
	var wg sync.WaitGroup
	for i := 0; i < presses; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctl.Next(ctx)
		}()
		time.Sleep(20 * time.Millisecond) // ~5 presses in a second
	}
	wg.Wait()
	waitFor(t, 3*time.Second, func() bool { return len(rec.calls()) >= presses })

	want := []string{"Up Next", "Second Up", "Third Up", "Q4", "Q5"}
	got := make([]string, 0, presses)
	for _, c := range rec.calls()[:presses] {
		if c.pending {
			t.Fatalf("the lookahead ran dry: %v", titlesOf(rec.calls()))
		}
		got = append(got, c.np.Title)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("press %d named %q, want %q (all: %v)", i+1, got[i], want[i], got)
		}
	}
}

// Concurrent presses must never both claim the same track.
func TestConcurrentSkipsClaimDistinctTracks(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctl.Reconfigure(Options{Step: 5, Debounce: 50 * time.Millisecond,
		Resync: time.Hour, TrackGuard: 0})
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ctl.Next(ctx) }()
	}
	wg.Wait()
	waitFor(t, 3*time.Second, func() bool { return len(rec.calls()) >= 6 })

	seen := map[string]bool{}
	for _, c := range rec.calls()[:6] {
		if c.pending {
			continue
		}
		if seen[c.np.Title] {
			t.Fatalf("track %q was claimed twice: %v", c.np.Title, titlesOf(rec.calls()))
		}
		seen[c.np.Title] = true
	}
}

func titlesOf(calls []trackCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.np.Title
		if c.pending {
			out[i] += "(pending)"
		}
	}
	return out
}

// The watcher reports what actually started playing through a separate path,
// so it can correct a card that is still up without announcing a track that
// is already playing as if it were coming next.
func TestWatcherReportsThroughConfirmation(t *testing.T) {
	f := newFakeAPI(t, 50)
	ctl := newTestController(t, f, 5, 50*time.Millisecond)
	ctl.Reconfigure(Options{Step: 5, Debounce: 50 * time.Millisecond,
		Resync: time.Hour, TrackGuard: time.Millisecond})
	rec := &recordingNotifier{}
	ctl.SetNotifier(rec)
	ctx := context.Background()

	if err := ctl.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ctl.refreshQueue(ctx)

	ctl.Next(ctx)
	// The fake player only reports a different track once we move it.
	f.mu.Lock()
	f.playing = "Up Next"
	f.mu.Unlock()

	waitFor(t, 3*time.Second, func() bool { return len(rec.confirmed()) > 0 })

	if got := len(rec.calls()); got != 1 {
		t.Fatalf("the press should account for exactly one card, got %d", got)
	}
	if got := rec.confirmed()[0].np.Title; got != "Up Next" {
		t.Fatalf("want the confirmation to name Up Next, got %q", got)
	}
}
