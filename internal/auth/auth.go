// Package auth implements the Spotify Authorization Code flow and keeps the
// access token fresh on disk.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	authorizeURL = "https://accounts.spotify.com/authorize"
	tokenURL     = "https://accounts.spotify.com/api/token"

	// Scopes needed to read and control playback.
	Scopes = "user-modify-playback-state user-read-playback-state"

	// refreshSkew is how long before expiry we proactively refresh.
	refreshSkew = 60 * time.Second
)

// Token is what we persist between runs.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope,omitempty"`
}

func (t Token) valid() bool {
	return t.AccessToken != "" && time.Now().Add(refreshSkew).Before(t.Expiry)
}

// Manager owns the token lifecycle. It is safe for concurrent use.
type Manager struct {
	clientID     string
	clientSecret string
	redirectURI  string
	path         string
	log          *slog.Logger
	hc           *http.Client

	mu  sync.Mutex
	tok Token
}

// New returns a Manager storing its token next to the config file.
func New(clientID, clientSecret, redirectURI, dir string, log *slog.Logger) *Manager {
	return &Manager{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		path:         filepath.Join(dir, "token.json"),
		log:          log,
		hc:           &http.Client{Timeout: 15 * time.Second},
	}
}

// TokenPath is where the refresh token lives.
func (m *Manager) TokenPath() string { return m.path }

// Load reads a previously saved token. A missing file is not an error.
func (m *Manager) Load() error {
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Unmarshal(b, &m.tok)
}

// HasRefreshToken reports whether we can run without user interaction.
func (m *Manager) HasRefreshToken() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok.RefreshToken != ""
}

func (m *Manager) save() error {
	b, err := json.MarshalIndent(m.tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(b, '\n'), 0o600)
}

// AccessToken returns a valid access token, refreshing it before it expires.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok.valid() {
		return m.tok.AccessToken, nil
	}
	if m.tok.RefreshToken == "" {
		return "", errors.New("not authorized yet: run spotify-knob -auth")
	}
	if err := m.refreshLocked(ctx); err != nil {
		return "", err
	}
	return m.tok.AccessToken, nil
}

// Invalidate forces the next AccessToken call to refresh. Used after a 401.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok.Expiry = time.Time{}
}

func (m *Manager) refreshLocked(ctx context.Context) error {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {m.tok.RefreshToken},
	}
	tr, err := m.postToken(ctx, form)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	m.tok.AccessToken = tr.AccessToken
	m.tok.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.Scope != "" {
		m.tok.Scope = tr.Scope
	}
	// Spotify only sometimes rotates the refresh token; keep the old one otherwise.
	if tr.RefreshToken != "" {
		m.tok.RefreshToken = tr.RefreshToken
	}
	m.log.Info("access token refreshed", "expires_in", tr.ExpiresIn)
	return m.save()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (m *Manager) postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(m.clientID, m.clientSecret)

	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK || tr.Error != "" {
		return nil, fmt.Errorf("status %d: %s %s", resp.StatusCode, tr.Error, tr.ErrorDesc)
	}
	return &tr, nil
}

// Authorize runs the one-time browser flow: it spins up a temporary HTTP
// server on the redirect URI's port, opens the browser, catches the callback
// and exchanges the code for a token. The port must be free, so this runs
// before the daemon's own listener starts.
func (m *Manager) Authorize(ctx context.Context) error {
	u, err := url.Parse(m.redirectURI)
	if err != nil {
		return fmt.Errorf("bad redirect URI: %w", err)
	}
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		return fmt.Errorf("listen on %s for the OAuth callback: %w", u.Host, err)
	}
	defer ln.Close()

	state, err := randomState()
	if err != nil {
		return err
	}

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			writePage(w, "Authorization failed", e)
			done <- result{err: fmt.Errorf("spotify returned error: %s", e)}
			return
		}
		if q.Get("state") != state {
			writePage(w, "Authorization failed", "state mismatch")
			done <- result{err: errors.New("state mismatch (possible CSRF), aborting")}
			return
		}
		writePage(w, "Authorized", "spotify-knob is connected. You can close this tab.")
		done <- result{code: q.Get("code")}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "spotify-knob: waiting for the Spotify callback", http.StatusNotFound)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := authorizeURL + "?" + url.Values{
		"client_id":     {m.clientID},
		"response_type": {"code"},
		"redirect_uri":  {m.redirectURI},
		"scope":         {Scopes},
		"state":         {state},
	}.Encode()

	fmt.Println("Opening your browser to authorize spotify-knob...")
	fmt.Println("If it does not open, paste this URL manually:")
	fmt.Println("  " + authURL)
	if err := openBrowser(authURL); err != nil {
		m.log.Warn("could not open browser automatically", "err", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	select {
	case <-ctx.Done():
		return errors.New("timed out waiting for the Spotify callback")
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		return m.exchange(ctx, r.code)
	}
}

// Exchange trades an authorization code for a token. Authorize does this
// automatically; it is exported for the case where the browser landed on the
// callback with nothing listening, leaving a still-valid code in the URL bar.
func (m *Manager) Exchange(ctx context.Context, code string) error {
	return m.exchange(ctx, code)
}

func (m *Manager) exchange(ctx context.Context, code string) error {
	tr, err := m.postToken(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {m.redirectURI},
	})
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok = Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
		Scope:        tr.Scope,
	}
	if err := m.save(); err != nil {
		return err
	}
	m.log.Info("authorized", "token_file", m.path, "scope", tr.Scope)
	return nil
}

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writePage(w http.ResponseWriter, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const page = `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="font-family:system-ui;background:#121212;color:#fff;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1 style="color:#1db954">%s</h1><p>%s</p></div>`
	fmt.Fprintf(w, page, title, title, msg)
}

func openBrowser(u string) error {
	// rundll32 avoids cmd.exe quoting rules mangling the query string.
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}
