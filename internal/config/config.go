// Package config handles on-disk configuration for the spotify-knob daemon.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AppDirName is the folder created under %APPDATA%.
const AppDirName = "spotify-knob"

// Config is the user-editable configuration file.
type Config struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	Port          int    `json:"port"`
	VolumeStep    int    `json:"volume_step"`
	DebounceMS    int    `json:"debounce_ms"`
	ResyncSeconds int    `json:"resync_seconds"`
	TrackGuardMS  int    `json:"track_guard_ms"`
	// DoublePressMS of 0 means a press skips immediately and previous lives
	// on Alt+press. Any positive value brings double-press back, at the cost
	// of delaying every skip by that long and of reading a fast run of
	// presses as double-presses.
	DoublePressMS int    `json:"double_press_ms"`
	LongPressMS   int    `json:"long_press_ms"`
	PeekLingerMS  int    `json:"peek_linger_ms"`
	PeekBrowseMS  int    `json:"peek_browse_ms"`
	PeekGesture   string `json:"peek_gesture"`
	Hotkeys       bool   `json:"hotkeys"`
	OSD           OSD    `json:"osd"`
	Lyrics        Lyrics `json:"lyrics"`
}

// Lyrics configures the floating lyrics panel. Its position and size are not
// here: those are moved by dragging, and writing them back into a file the
// user edits by hand would fight with the hot reload. They live in
// lyrics-window.json beside this file instead.
type Lyrics struct {
	Enabled bool    `json:"enabled"`
	Opacity float64 `json:"opacity"` // 0.5-1
	Scale   float64 `json:"scale"`   // 0 follows the system DPI
	FPS     int     `json:"fps"`     // 0 follows the monitor
}

// OSD configures the on-screen card.
type OSD struct {
	Enabled            bool    `json:"enabled"`
	Scale              float64 `json:"scale"`
	Position           string  `json:"position"`
	HideWhenFullscreen bool    `json:"hide_when_fullscreen"`
	DismissOnClick     bool    `json:"dismiss_on_click"`
	VolumeHoldMS       int     `json:"volume_hold_ms"`
	TrackHoldMS        int     `json:"track_hold_ms"`
	FPS                int     `json:"fps"`
}

func (o OSD) VolumeHold() time.Duration {
	return time.Duration(o.VolumeHoldMS) * time.Millisecond
}

func (o OSD) TrackHold() time.Duration {
	return time.Duration(o.TrackHoldMS) * time.Millisecond
}

// Default returns the config used when a field is missing from the file.
func Default() Config {
	return Config{
		Port:          8888,
		VolumeStep:    5,
		DebounceMS:    100,
		ResyncSeconds: 10,
		TrackGuardMS:  150,
		DoublePressMS: 0,
		LongPressMS:   450,
		PeekLingerMS:  1200,
		PeekBrowseMS:  1800,
		PeekGesture:   "alt-turn",
		Hotkeys:       true,
		OSD: OSD{
			Enabled:            true,
			Scale:              1,
			Position:           "bottom-center",
			HideWhenFullscreen: true,
			DismissOnClick:     true,
			VolumeHoldMS:       1500,
			TrackHoldMS:        3000,
			FPS:                0, // follow the monitor's refresh rate
		},
		Lyrics: Lyrics{
			Enabled: true,
			Opacity: 0.94,
			Scale:   0,
			FPS:     0,
		},
	}
}

// Dir returns %APPDATA%\spotify-knob, creating it if needed.
func Dir() (string, error) {
	base := os.Getenv("APPDATA")
	if base == "" {
		var err error
		if base, err = os.UserConfigDir(); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(base, AppDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Path returns the default config file location.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads path. Decoding on top of the defaults means a key absent from
// the file keeps its default, while a key present in the file wins even when
// its value is the zero value - which is what makes "hotkeys": false work.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	b, err = stripBOM(b)
	if err != nil {
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}

// stripBOM removes a UTF-8 byte order mark.
//
// Notepad and PowerShell both write one by default, and Go's JSON decoder
// rejects it outright. Without this, editing the config in the most obvious
// way on Windows leaves a file the daemon refuses to start with.
func stripBOM(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return b[3:], nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		// UTF-16 needs more than a trim; say so plainly instead of failing
		// with a confusing JSON error.
		return nil, errors.New("file is UTF-16; save it as UTF-8")
	}
	return b, nil
}

// Save writes the config with owner-only permissions (it holds the client secret).
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Validate checks the fields the daemon cannot run without.
func (c Config) Validate() error {
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("client_id and client_secret must be set in config.json")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if c.VolumeStep < 1 || c.VolumeStep > 50 {
		return fmt.Errorf("volume_step %d out of range (1-50)", c.VolumeStep)
	}
	return nil
}

// RedirectURI must match the URI registered in the Spotify dashboard exactly.
func (c Config) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", c.Port)
}

// Addr is the loopback-only listen address. Never bind 0.0.0.0.
func (c Config) Addr() string { return fmt.Sprintf("127.0.0.1:%d", c.Port) }

func (c Config) Debounce() time.Duration    { return time.Duration(c.DebounceMS) * time.Millisecond }
func (c Config) Resync() time.Duration      { return time.Duration(c.ResyncSeconds) * time.Second }
func (c Config) TrackGuard() time.Duration  { return time.Duration(c.TrackGuardMS) * time.Millisecond }
func (c Config) DoublePress() time.Duration { return time.Duration(c.DoublePressMS) * time.Millisecond }
func (c Config) LongPress() time.Duration   { return time.Duration(c.LongPressMS) * time.Millisecond }
func (c Config) PeekLinger() time.Duration  { return time.Duration(c.PeekLingerMS) * time.Millisecond }
func (c Config) PeekBrowse() time.Duration  { return time.Duration(c.PeekBrowseMS) * time.Millisecond }
