// Command spotify-knob turns a keyboard's media keys into Spotify-only
// controls: rotate the knob to change Spotify's volume via the Web API, press
// it to skip, double-press to go back.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"spotify-knob/internal/auth"
	"spotify-knob/internal/config"
	"spotify-knob/internal/controller"
	"spotify-knob/internal/hotkey"
	"spotify-knob/internal/lyrics"
	"spotify-knob/internal/osd"
	"spotify-knob/internal/server"
	"spotify-knob/internal/spotify"
)

const version = "1.0.0"

// maxLogBytes caps the log file; it is truncated on start once it grows past this.
const maxLogBytes = 5 << 20

func main() {
	var (
		cfgPath   = flag.String("config", "", "path to config.json (default %APPDATA%\\spotify-knob\\config.json)")
		verbose   = flag.Bool("verbose", false, "log every knob event and API call")
		doAuth    = flag.Bool("auth", false, "run the Spotify authorization flow and exit")
		pasteCode = flag.String("code", "", "finish authorization with a code copied from the callback URL, then exit")
		noHotkey  = flag.Bool("no-hotkeys", false, "do not install the keyboard hook (HTTP endpoints only)")
		showVer   = flag.Bool("version", false, "print version and exit")
		status    = flag.Bool("status", false, "query a running daemon and exit")
		preview   = flag.Bool("preview", false, "show sample on-screen cards and exit, without touching Spotify")
		previewLy = flag.String("preview-lyrics", "", "open the lyrics panel on \"artist - title\" and stay up, without touching Spotify")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("spotify-knob", version)
		return
	}

	if *previewLy != "" {
		if err := runLyricsPreview(*cfgPath, *previewLy, *verbose); err != nil {
			fmt.Fprintln(os.Stderr, "spotify-knob:", err)
			os.Exit(1)
		}
		return
	}

	if *preview {
		if err := runPreview(*cfgPath, *verbose); err != nil {
			fmt.Fprintln(os.Stderr, "spotify-knob:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*cfgPath, *verbose, *doAuth, *noHotkey, *status, *pasteCode); err != nil {
		fmt.Fprintln(os.Stderr, "spotify-knob:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, verbose, doAuth, noHotkey, statusOnly bool, pasteCode string) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, "config.json")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := writeTemplate(cfgPath); err != nil {
				return err
			}
			return fmt.Errorf("no config yet: a template was written to %s, fill in client_id and client_secret", cfgPath)
		}
		return err
	}

	if statusOnly {
		return printStatus(cfg)
	}

	log, closeLog, err := newLogger(dir, verbose)
	if err != nil {
		return err
	}
	defer closeLog()

	// One line saying what was actually loaded. Cheap, and it turns "my config
	// edit did nothing" from a mystery into a glance at the log.
	if fi, statErr := os.Stat(cfgPath); statErr == nil {
		log.Info("config loaded",
			"path", cfgPath,
			"size", fi.Size(),
			"modified", fi.ModTime().Format(time.RFC3339),
			"debounce_ms", cfg.DebounceMS,
			"volume_step", cfg.VolumeStep,
			"osd_enabled", cfg.OSD.Enabled)
	}

	authMgr := auth.New(cfg.ClientID, cfg.ClientSecret, cfg.RedirectURI(), dir, log)
	if err := authMgr.Load(); err != nil {
		log.Warn("could not read saved token, re-authorizing", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if pasteCode != "" {
		if err := authMgr.Exchange(ctx, pasteCode); err != nil {
			return err
		}
		fmt.Println("Authorized. Token saved to", authMgr.TokenPath())
		return nil
	}

	if doAuth || !authMgr.HasRefreshToken() {
		// The callback server needs the daemon's port, so authorize first.
		if err := authMgr.Authorize(ctx); err != nil {
			return err
		}
		fmt.Println("Authorized. Token saved to", authMgr.TokenPath())
		if doAuth {
			return nil
		}
	}

	client := spotify.New(authMgr, log)
	ctl := controller.New(client, log, controllerOptions(cfg))
	srv := server.New(cfg.Addr(), ctl, server.Info{
		Version:    version,
		ConfigPath: cfgPath,
		OSDEnabled: cfg.OSD.Enabled,
		OSDFPS:     cfg.OSD.FPS,
	}, log)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ctl.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Also to the log, not just to the error return. The windowless
			// build has no stderr to read, so "another daemon already has the
			// port" would otherwise show up as nothing but a process that
			// quietly stops before the overlay ever starts.
			log.Error("http listener failed", "addr", srv.Addr(), "err", err)
			serveErr <- err
		}
		close(serveErr)
	}()

	card := osd.New(osdOptions(cfg), log)
	queueLen := &atomic.Int64{}
	bridge := osdBridge{card: card, queueLen: queueLen}
	ctl.SetNotifier(bridge)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := card.Run(ctx); err != nil {
			log.Error("on-screen display unavailable", "err", err)
		}
	}()

	store := newPanelStore(dir, log)
	panelOpts := lyricsOptions(cfg, store)
	panelOpts.OnSeek = func(pos time.Duration) { ctl.Seek(context.WithoutCancel(ctx), pos) }
	panel := osd.NewLyrics(panelOpts, log)
	lyr := newLyricsManager(ctl, panel, card,
		lyrics.New(filepath.Join(dir, "lyrics"), log), log)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := panel.Run(ctx); err != nil {
			log.Error("lyrics panel unavailable", "err", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		lyr.run(ctx)
	}()

	gestures := &atomic.Pointer[gestureConfig]{}
	gestures.Store(gestureFrom(cfg))

	if cfg.Hotkeys && !noHotkey {
		hk := hotkey.New(log)
		router := newKnobRouter(ctl, card, gestures, queueLen,
			hotkey.AltPressed, hotkey.CtrlPressed, lyr, log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			router.run(ctx, hk.Events())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hk.Run(ctx); err != nil {
				log.Error("keyboard hook failed, falling back to HTTP only", "err", err)
			}
		}()
	} else {
		log.Info("keyboard hook disabled, HTTP endpoints only")
	}

	// Config changes take effect without a restart. Anything the daemon
	// cannot adopt live (the port, the credentials) is called out in the log
	// rather than silently ignored.
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchConfig(ctx, cfgPath, cfg, log, func(next config.Config) {
			ctl.Reconfigure(controllerOptions(next))
			card.Reconfigure(osdOptions(next))
			// A config edit is an explicit statement, so its opacity wins over
			// whatever the slider was last dragged to.
			nextPanel := lyricsOptions(next, store)
			nextPanel.X, nextPanel.Y, nextPanel.W, nextPanel.H = 0, 0, 0, 0
			nextPanel.Opacity = next.Lyrics.Opacity
			nextPanel.OnSeek = panelOpts.OnSeek
			panel.Reconfigure(nextPanel)
			gestures.Store(gestureFrom(next))
		})
	}()

	fmt.Printf("spotify-knob %s running on http://%s (Ctrl+C to stop)\n", version, srv.Addr())
	fmt.Println("  knob turn  -> Spotify volume    knob press -> next    double press -> previous")
	fmt.Println("  Alt+press  -> previous (no wait)    Alt+turn -> browse the queue, press to play")
	fmt.Println("  Ctrl+press -> lyrics panel (drag to move, corner to resize)")
	fmt.Println("  hold Shift while turning to pass the key through to Windows volume")
	if cfg.OSD.Enabled {
		fmt.Println("  on-screen card:", cfg.OSD.Position+", hidden while an app is fullscreen")
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			stop()
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
	wg.Wait()
	log.Info("stopped")
	return nil
}

// runPreview shows the cards with stand-in content. It needs no token and
// touches no playback, so it is the fastest way to check where the card lands
// and how it looks at the configured scale.
// runLyricsPreview opens the panel on a named track and leaves it up, so the
// panel's design can be worked on without waiting for the right song to come
// round on the real player. The argument is "artist - title".
func runLyricsPreview(cfgPath, want string, verbose bool) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		cfg = config.Default()
	}

	log, closeLog, err := newLogger(dir, verbose)
	if err != nil {
		return err
	}
	defer closeLog()

	artist, title := "", want
	if i := strings.Index(want, " - "); i > 0 {
		artist, title = want[:i], want[i+3:]
	}

	opts := lyricsOptions(cfg, newPanelStore(dir, log))
	opts.Enabled = true
	panel := osd.NewLyrics(opts, log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- panel.Run(ctx) }()

	prov := lyrics.New(filepath.Join(dir, "lyrics"), log)
	doc, err := prov.Get(ctx, lyrics.Query{Title: title, Artist: artist})
	if err != nil && !errors.Is(err, lyrics.ErrNotFound) {
		return err
	}

	total := 3*time.Minute + 30*time.Second
	if doc != nil && len(doc.Lines) > 0 && doc.Synced {
		total = doc.Lines[len(doc.Lines)-1].At + 12*time.Second
	}
	track := osd.LyricsTrack{
		Title: title, Artist: artist,
		URI:        "preview:" + want,
		Duration:   total,
		PositionAt: time.Now(),
		Playing:    true,
	}
	panel.SetTrack(track)
	if doc == nil {
		panel.Missing(track.URI)
		fmt.Println("No lyrics found for", want)
	} else {
		panel.Ready(track.URI, convertDoc(doc))
		fmt.Printf("%d lines, synced=%v. Ctrl+C to close.\n", len(doc.Lines), doc.Synced)
	}
	panel.Show()

	// Keep the playhead moving so the highlight and the scroll can be seen.
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(2 * time.Second):
				return nil
			}
		case <-tick.C:
			panel.SetTrack(track)
		}
	}
}

func runPreview(cfgPath string, verbose bool) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A missing client ID does not matter here; keep going with defaults.
		cfg = config.Default()
	}

	log, closeLog, err := newLogger(dir, verbose)
	if err != nil {
		return err
	}
	defer closeLog()

	card := osd.New(osd.Options{
		Enabled:        true,
		Scale:          cfg.OSD.Scale,
		Position:       cfg.OSD.Position,
		HideFullscreen: false, // previewing is an explicit request; always show
		VolumeHold:     cfg.OSD.VolumeHold(),
		TrackHold:      cfg.OSD.TrackHold(),
		FPS:            cfg.OSD.FPS,
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- card.Run(ctx) }()

	// Durations are set so the preview exercises the progress row too; a
	// track with no length known draws no footer at all.
	demo := osd.Track{
		Title: "Weird Fishes / Arpeggi", Artist: "Radiohead",
		Duration: 5*time.Minute + 18*time.Second,
		Position: 1*time.Minute + 54*time.Second,
		Playing:  true,
	}
	fmt.Println("Showing sample cards...")

	for _, v := range []int{45, 50, 55, 60, 65} {
		card.ShowVolume(v, demo)
		time.Sleep(140 * time.Millisecond)
	}
	time.Sleep(2200 * time.Millisecond)

	card.ShowTrack(osd.Forward, osd.Track{
		Title: "Sunset Lover", Artist: "Petit Biscuit",
		Duration: 3*time.Minute + 58*time.Second, Position: 12 * time.Second, Playing: true,
	}, false)
	time.Sleep(3400 * time.Millisecond)

	card.ShowTrack(osd.Backward, osd.Track{
		Title: "Redbone", Artist: "Childish Gambino",
		Duration: 5*time.Minute + 27*time.Second, Position: 4*time.Minute + 3*time.Second, Playing: true,
	}, false)
	time.Sleep(3400 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return nil
	}
}

// osdBridge adapts controller notifications to the overlay, keeping the
// controller free of any window code.
type osdBridge struct {
	card     *osd.OSD
	queueLen *atomic.Int64
}

func osdTrack(np controller.NowPlaying) osd.Track {
	return osd.Track{
		Title:      np.Title,
		Artist:     np.Artist,
		ArtURL:     np.ArtURL,
		URI:        np.URI,
		Duration:   np.Duration,
		Position:   np.Position,
		PositionAt: np.PositionAt,
		Playing:    np.Playing,
	}
}

func (b osdBridge) TrackConfirmed(forward bool, np controller.NowPlaying) {
	dir := osd.Forward
	if !forward {
		dir = osd.Backward
	}
	b.card.CorrectTrack(dir, osdTrack(np))
}

func (b osdBridge) QueueChanged(q []controller.NowPlaying) {
	tracks := make([]osd.Track, len(q))
	for i, np := range q {
		tracks[i] = osdTrack(np)
	}
	b.queueLen.Store(int64(len(tracks)))
	b.card.SetQueue(tracks)
}

func (b osdBridge) VolumeChanged(volume int, np controller.NowPlaying) {
	b.card.ShowVolume(volume, osdTrack(np))
}

func (b osdBridge) TrackChanged(forward bool, np controller.NowPlaying, pending bool) {
	dir := osd.Forward
	if !forward {
		dir = osd.Backward
	}
	b.card.ShowTrack(dir, osdTrack(np), pending)
}

func controllerOptions(cfg config.Config) controller.Options {
	return controller.Options{
		Step:       cfg.VolumeStep,
		Debounce:   cfg.Debounce(),
		Resync:     cfg.Resync(),
		TrackGuard: cfg.TrackGuard(),
	}
}

func osdOptions(cfg config.Config) osd.Options {
	return osd.Options{
		Enabled:        cfg.OSD.Enabled,
		Scale:          cfg.OSD.Scale,
		Position:       cfg.OSD.Position,
		HideFullscreen: cfg.OSD.HideWhenFullscreen,
		DismissOnClick: cfg.OSD.DismissOnClick,
		VolumeHold:     cfg.OSD.VolumeHold(),
		TrackHold:      cfg.OSD.TrackHold(),
		FPS:            cfg.OSD.FPS,
	}
}

// lyricsOptions folds the config and the remembered window position into the
// panel's options. Geometry does not come from the config file: it is set by
// dragging, and a hot reload must not yank a panel back to where the file
// last said it was.
func lyricsOptions(cfg config.Config, store *panelStore) osd.LyricsOptions {
	g := store.load()
	opts := osd.LyricsOptions{
		Enabled:    cfg.Lyrics.Enabled,
		Opacity:    cfg.Lyrics.Opacity,
		Scale:      cfg.Lyrics.Scale,
		FPS:        cfg.Lyrics.FPS,
		X:          g.X,
		Y:          g.Y,
		W:          g.W,
		H:          g.H,
		OnGeometry: store.Geometry,
		OnOpacity:  store.Opacity,
	}
	// A value dragged on the slider outlives a restart, and outranks the
	// config default it started from.
	if g.Opacity > 0 {
		opts.Opacity = g.Opacity
	}
	return opts
}

func gestureFrom(cfg config.Config) *gestureConfig {
	return &gestureConfig{
		doublePress: cfg.DoublePress(),
		longPress:   cfg.LongPress(),
		peekLinger:  cfg.PeekLinger(),
		peekBrowse:  cfg.PeekBrowse(),
		peek:        parsePeekGesture(cfg.PeekGesture),
	}
}

// watchConfig polls the config file and republishes it when it changes.
//
// Polling rather than a filesystem watcher: one stat every two seconds is
// nothing, and it behaves the same whether the file is edited in place,
// replaced, or written by another tool.
func watchConfig(ctx context.Context, path string, initial config.Config,
	log *slog.Logger, apply func(config.Config)) {

	var size int64
	var mod time.Time
	if fi, err := os.Stat(path); err == nil {
		size, mod = fi.Size(), fi.ModTime()
	}
	prev := initial

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fi, err := os.Stat(path)
			if err != nil || (fi.Size() == size && fi.ModTime().Equal(mod)) {
				continue
			}
			// Remember what we looked at even when it fails to parse, so a
			// broken file is reported once rather than every two seconds.
			size, mod = fi.Size(), fi.ModTime()

			next, err := config.Load(path)
			if err != nil {
				// Probably caught mid-write, or genuinely broken. Keep what we
				// have; the next edit gets another go.
				log.Warn("config reload failed, keeping the running settings", "err", err)
				continue
			}

			if next.Port != prev.Port || next.ClientID != prev.ClientID ||
				next.Hotkeys != prev.Hotkeys {
				log.Warn("config changed in a way that needs a restart",
					"port", next.Port, "hotkeys", next.Hotkeys)
			}
			log.Info("config reloaded",
				"volume_step", next.VolumeStep,
				"debounce_ms", next.DebounceMS,
				"osd_enabled", next.OSD.Enabled,
				"osd_scale", next.OSD.Scale)
			prev = next
			apply(next)
		}
	}
}

func writeTemplate(path string) error {
	c := config.Default()
	c.ClientID = "PASTE_YOUR_CLIENT_ID"
	c.ClientSecret = "PASTE_YOUR_CLIENT_SECRET"
	return c.Save(path)
}

func printStatus(cfg config.Config) error {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://" + cfg.Addr() + "/status")
	if err != nil {
		return fmt.Errorf("daemon not reachable on %s: %w", cfg.Addr(), err)
	}
	defer resp.Body.Close()
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	return nil
}

// newLogger writes to both the console and %APPDATA%\spotify-knob\daemon.log.
func newLogger(dir string, verbose bool) (*slog.Logger, func(), error) {
	logPath := filepath.Join(dir, "daemon.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogBytes {
		os.Rename(logPath, logPath+".old")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	w := io.MultiWriter(f, os.Stderr)
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h), func() { f.Close() }, nil
}
