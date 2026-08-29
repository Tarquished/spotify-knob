package controller

import "testing"

func np(title string) NowPlaying { return NowPlaying{Title: title, Artist: "A"} }

func TestHistoryEmptyHasNoPrevious(t *testing.T) {
	h := newTrackHistory(8)
	if _, ok := h.previous(); ok {
		t.Fatal("empty history should have no previous track")
	}
	h.observe(np("one"))
	if _, ok := h.previous(); ok {
		t.Fatal("a single track has nothing before it")
	}
}

func TestHistoryWalksBackwards(t *testing.T) {
	h := newTrackHistory(8)
	h.observe(np("one"))
	h.observe(np("two"))
	h.observe(np("three"))

	got, ok := h.previous()
	if !ok || got.Title != "two" {
		t.Fatalf("want two, got %v (ok=%v)", got.Title, ok)
	}

	// Actually going back must move the cursor, so the next press walks
	// further rather than pointing at the track we just left.
	h.observe(np("two"))
	got, ok = h.previous()
	if !ok || got.Title != "one" {
		t.Fatalf("after stepping back, want one, got %v (ok=%v)", got.Title, ok)
	}

	h.observe(np("one"))
	if _, ok := h.previous(); ok {
		t.Fatal("at the start of history there is nothing before")
	}
}

func TestHistoryStepForwardOverKnownGround(t *testing.T) {
	h := newTrackHistory(8)
	h.observe(np("one"))
	h.observe(np("two"))
	h.observe(np("three"))
	h.observe(np("two")) // back
	h.observe(np("three"))

	got, ok := h.previous()
	if !ok || got.Title != "two" {
		t.Fatalf("want two after returning forward, got %v (ok=%v)", got.Title, ok)
	}
}

// Branching off mid-history must discard what was ahead, otherwise previous
// would offer a track that is no longer reachable.
func TestHistoryBranchDiscardsFuture(t *testing.T) {
	h := newTrackHistory(8)
	h.observe(np("one"))
	h.observe(np("two"))
	h.observe(np("three"))
	h.observe(np("two"))   // back
	h.observe(np("fresh")) // somewhere new
	h.observe(np("newer"))

	got, _ := h.previous()
	if got.Title != "fresh" {
		t.Fatalf("want fresh, got %v", got.Title)
	}
	h.observe(np("fresh"))
	got, _ = h.previous()
	if got.Title != "two" {
		t.Fatalf("want two, got %v", got.Title)
	}
}

func TestHistoryIgnoresRepeatsAndBlanks(t *testing.T) {
	h := newTrackHistory(8)
	h.observe(np("one"))
	h.observe(np("one"))
	h.observe(NowPlaying{})
	h.observe(np("two"))

	if got, ok := h.previous(); !ok || got.Title != "one" {
		t.Fatalf("want one, got %v (ok=%v)", got.Title, ok)
	}
	if len(h.items) != 2 {
		t.Fatalf("want 2 entries, got %d", len(h.items))
	}
}

func TestHistoryDropsOldestPastTheCap(t *testing.T) {
	h := newTrackHistory(3)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		h.observe(np(name))
	}
	if len(h.items) != 3 {
		t.Fatalf("want 3 entries after the cap, got %d", len(h.items))
	}
	if h.items[h.pos].Title != "e" {
		t.Fatalf("cursor should stay on the newest track, got %v", h.items[h.pos].Title)
	}
	if got, _ := h.previous(); got.Title != "d" {
		t.Fatalf("want d, got %v", got.Title)
	}
}
