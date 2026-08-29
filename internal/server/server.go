// Package server exposes the daemon on loopback so anything else (an AHK
// script, curl, a Stream Deck) can drive the same controller as the built-in
// keyboard hook.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"spotify-knob/internal/controller"
)

// Info is what the daemon reports about itself. It exists because "the
// running daemon disagrees with the config file on disk" is otherwise
// impossible to diagnose from the outside.
type Info struct {
	Version    string `json:"version"`
	ConfigPath string `json:"config_path"`
	OSDEnabled bool   `json:"osd_enabled"`
	OSDFPS     int    `json:"osd_fps"`
}

type Server struct {
	ctl  *controller.Controller
	info Info
	log  *slog.Logger
	http *http.Server
}

func New(addr string, ctl *controller.Controller, info Info, log *slog.Logger) *Server {
	s := &Server{ctl: ctl, info: info, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /volume/up", s.volume(+1))
	mux.HandleFunc("POST /volume/down", s.volume(-1))
	mux.HandleFunc("POST /next", s.track(true))
	mux.HandleFunc("POST /previous", s.track(false))
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /", s.index)

	s.http = &http.Server{
		Addr:              addr, // always 127.0.0.1, never 0.0.0.0
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) Addr() string { return s.http.Addr }

// ListenAndServe blocks until the server stops.
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown stops accepting and drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) volume(delta int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.ctl.Adjust(r.Context(), delta)
		writeJSON(w, http.StatusAccepted, map[string]any{"target": s.ctl.Status().Target})
	}
}

func (s *Server) track(next bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if next {
			s.ctl.Next(r.Context())
		} else {
			s.ctl.Previous(r.Context())
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		controller.Status
		Info
	}{s.ctl.Status(), s.info})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	const help = `spotify-knob daemon

POST /volume/up
POST /volume/down
POST /next
POST /previous
GET  /status
`
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(help))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
