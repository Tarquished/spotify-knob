package controller

// trackHistory records what has been playing, so a "previous" press can name
// the track it is going back to without waiting for Spotify to switch.
//
// A plain list is not enough. Pressing previous twice has to walk further
// back, not keep reporting the track that was just left, so the history keeps
// a cursor and moves it when it recognises the newly playing track as a
// neighbour of the current position.
type trackHistory struct {
	items []NowPlaying
	pos   int // index of what is playing now; -1 when empty
	max   int
}

func newTrackHistory(max int) *trackHistory {
	if max < 2 {
		max = 2
	}
	return &trackHistory{pos: -1, max: max}
}

// observe records the track that is playing now.
func (h *trackHistory) observe(np NowPlaying) {
	if np.Title == "" {
		return
	}
	k := np.key()
	switch {
	case h.pos >= 0 && h.items[h.pos].key() == k:
		return // still the same track
	case h.pos > 0 && h.items[h.pos-1].key() == k:
		h.pos-- // stepped back
		return
	case h.pos >= 0 && h.pos+1 < len(h.items) && h.items[h.pos+1].key() == k:
		h.pos++ // stepped forward over ground already covered
		return
	}

	// Somewhere new: whatever was ahead of the cursor is no longer reachable.
	h.items = append(h.items[:h.pos+1], np)
	h.pos = len(h.items) - 1

	if len(h.items) > h.max {
		drop := len(h.items) - h.max
		h.items = append(h.items[:0], h.items[drop:]...)
		h.pos -= drop
	}
}

// previous is the track a "previous" press should land on.
func (h *trackHistory) previous() (NowPlaying, bool) {
	if h.pos <= 0 {
		return NowPlaying{}, false
	}
	return h.items[h.pos-1], true
}
