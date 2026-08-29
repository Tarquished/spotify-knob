// Package lyrics finds time-synced lyrics for the track that is playing.
//
// Spotify's Web API does not serve lyrics at all. The lyrics inside the
// Spotify app come from Musixmatch through an internal endpoint that is
// authorised by a web-player session cookie, not by the OAuth token this
// daemon holds, so there is nothing to ask for with the credentials we have.
//
// LRCLIB (lrclib.net) is used instead: a free, key-less, community database
// that returns LRC - one timestamp per line - which is exactly the shape a
// highlight-follows-the-playhead view needs.
package lyrics

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Line is one lyric line and the moment it starts.
type Line struct {
	At   time.Duration
	Text string // empty means an instrumental gap, drawn as a rest
}

// Lyrics is a whole track's words.
type Lyrics struct {
	Lines        []Line
	Synced       bool // timestamps are real; false means Lines are evenly ordered text only
	Instrumental bool
	Title        string
	Artist       string
	Source       string
}

// Empty reports whether there is nothing worth showing.
func (l *Lyrics) Empty() bool { return l == nil || len(l.Lines) == 0 }

// IndexAt is the line that should be highlighted at playhead position pos.
// It returns -1 before the first line starts, which is the intro.
func (l *Lyrics) IndexAt(pos time.Duration) int {
	if l == nil || len(l.Lines) == 0 || !l.Synced {
		return -1
	}
	// Binary search for the last line whose timestamp has passed.
	lo, hi := 0, len(l.Lines)
	for lo < hi {
		mid := (lo + hi) / 2
		if l.Lines[mid].At <= pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

// EndOf is when the line at index i gives way to the next one. The last line
// is given a generous tail so it does not blink out the moment it lands.
func (l *Lyrics) EndOf(i int) time.Duration {
	if l == nil || i < 0 || i >= len(l.Lines) {
		return 0
	}
	if i+1 < len(l.Lines) {
		return l.Lines[i+1].At
	}
	return l.Lines[i].At + 10*time.Second
}

// parseLRC turns an LRC document into lines.
//
// Real-world LRC is loose: metadata tags share the bracket syntax with
// timestamps, one line of text can carry several timestamps when it repeats,
// and blank lines are meaningful (they are the instrumental gaps). All three
// are handled here rather than being left to the renderer.
func parseLRC(doc string) []Line {
	var out []Line
	for _, raw := range strings.Split(doc, "\n") {
		s := strings.TrimRight(raw, "\r")
		var stamps []time.Duration

		for strings.HasPrefix(s, "[") {
			end := strings.IndexByte(s, ']')
			if end < 0 {
				break
			}
			tag := s[1:end]
			at, ok := parseStamp(tag)
			if !ok {
				// A metadata tag such as [ar:...]. Nothing after it on this
				// line is lyric text either, so drop the whole line.
				stamps = nil
				s = ""
				break
			}
			stamps = append(stamps, at)
			s = s[end+1:]
		}
		if len(stamps) == 0 {
			continue
		}
		text := strings.TrimSpace(s)
		for _, at := range stamps {
			out = append(out, Line{At: at, Text: text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// parseStamp reads mm:ss.xx, mm:ss.xxx or mm:ss. It rejects anything else,
// which is how metadata tags are told apart from timestamps.
func parseStamp(tag string) (time.Duration, bool) {
	colon := strings.IndexByte(tag, ':')
	if colon <= 0 {
		return 0, false
	}
	min, err := strconv.Atoi(tag[:colon])
	if err != nil || min < 0 {
		return 0, false
	}
	rest := tag[colon+1:]

	secPart, fracPart := rest, ""
	if dot := strings.IndexAny(rest, ".:"); dot >= 0 {
		secPart, fracPart = rest[:dot], rest[dot+1:]
	}
	sec, err := strconv.Atoi(secPart)
	if err != nil || sec < 0 || sec > 59 {
		return 0, false
	}

	d := time.Duration(min)*time.Minute + time.Duration(sec)*time.Second
	if fracPart != "" {
		digits := 0
		frac := 0
		for _, r := range fracPart {
			if r < '0' || r > '9' {
				break
			}
			frac = frac*10 + int(r-'0')
			digits++
			if digits == 3 {
				break
			}
		}
		switch digits {
		case 1:
			d += time.Duration(frac) * 100 * time.Millisecond
		case 2:
			d += time.Duration(frac) * 10 * time.Millisecond
		case 3:
			d += time.Duration(frac) * time.Millisecond
		}
	}
	return d, true
}

// plainLines splits unsynced lyrics into displayable lines, keeping the blank
// ones so verses stay apart.
func plainLines(doc string) []Line {
	var out []Line
	for _, raw := range strings.Split(doc, "\n") {
		out = append(out, Line{Text: strings.TrimSpace(strings.TrimRight(raw, "\r"))})
	}
	// Trailing blanks are just padding from the source.
	for len(out) > 0 && out[len(out)-1].Text == "" {
		out = out[:len(out)-1]
	}
	return out
}
