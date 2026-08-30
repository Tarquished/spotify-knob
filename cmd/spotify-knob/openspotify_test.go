package main

import (
	"io"
	"log/slog"
	"testing"
)

func TestSpotifyWebFallbackConvertsKnownURITypes(t *testing.T) {
	cases := map[string]string{
		"spotify:track:6habFhsOp2NvshLv26DqMb":    "https://open.spotify.com/track/6habFhsOp2NvshLv26DqMb",
		"spotify:album:abc123":                    "https://open.spotify.com/album/abc123",
		"spotify:artist:xyz":                      "https://open.spotify.com/artist/xyz",
		"spotify:playlist:37i9dQZF1DXcBWIGoYBM5M": "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M",
		"spotify:show:abc":                        "https://open.spotify.com/show/abc",
		"spotify:episode:abc":                     "https://open.spotify.com/episode/abc",
	}
	for in, want := range cases {
		if got := spotifyWebFallback(in); got != want {
			t.Errorf("spotifyWebFallback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpotifyWebFallbackRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not a uri",
		"spotify:track",         // no id
		"spotify:track:",        // empty id
		"spotify:",              // nothing at all
		"http://already-a-url",  // not our scheme
		"spotify:local:abc:def", // a kind LRCLIB/Spotify never opens on the web
	}
	for _, in := range cases {
		if got := spotifyWebFallback(in); got != "" {
			t.Errorf("spotifyWebFallback(%q) = %q, want empty", in, got)
		}
	}
}

func TestSpotifyLaunchTargetsTriesTheAppFirst(t *testing.T) {
	got := spotifyLaunchTargets("spotify:track:abc")
	want := []string{"spotify:track:abc", "https://open.spotify.com/track/abc"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A preview-mode URI (not a real Spotify URI) has no web equivalent, so
// there is nothing to fall back to - only the one target is offered.
func TestSpotifyLaunchTargetsSkipsTheFallbackWhenThereIsNone(t *testing.T) {
	got := spotifyLaunchTargets("preview:Artist - Title")
	if len(got) != 1 || got[0] != "preview:Artist - Title" {
		t.Fatalf("want just the original target, got %v", got)
	}
}

// openInSpotify must not panic on an empty URI - the button that would call
// it is hidden in that case, but the function itself has to be safe too.
func TestOpenInSpotifyIgnoresAnEmptyURI(t *testing.T) {
	openInSpotify("", slog.New(slog.NewTextHandler(io.Discard, nil)))
}
