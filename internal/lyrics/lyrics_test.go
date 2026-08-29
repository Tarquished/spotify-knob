package lyrics

import (
	"testing"
	"time"
)

func TestParseLRCReadsTimestamps(t *testing.T) {
	lines := parseLRC("[00:06.20] Oh, ay\n[00:13.96] You don't know, babe\n[01:02.5]Halfway\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	want := []time.Duration{
		6*time.Second + 200*time.Millisecond,
		13*time.Second + 960*time.Millisecond,
		62*time.Second + 500*time.Millisecond,
	}
	for i, w := range want {
		if lines[i].At != w {
			t.Errorf("line %d: want %v, got %v", i, w, lines[i].At)
		}
	}
	if lines[1].Text != "You don't know, babe" {
		t.Errorf("text not trimmed: %q", lines[1].Text)
	}
}

// Metadata tags share the bracket syntax with timestamps and must not become
// lyric lines at time zero.
func TestParseLRCSkipsMetadata(t *testing.T) {
	lines := parseLRC("[ar:Daniel Caesar]\n[ti:Best Part]\n[length:03:29]\n[00:01.00]First\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d: %+v", len(lines), lines)
	}
	if lines[0].Text != "First" || lines[0].At != time.Second {
		t.Fatalf("got %+v", lines[0])
	}
}

// One line of text can carry several timestamps when it repeats.
func TestParseLRCExpandsRepeatedStamps(t *testing.T) {
	lines := parseLRC("[00:10.00][01:20.00]Chorus\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0].At != 10*time.Second || lines[1].At != 80*time.Second {
		t.Fatalf("stamps not expanded: %+v", lines)
	}
	if lines[0].Text != "Chorus" || lines[1].Text != "Chorus" {
		t.Fatal("both copies should carry the text")
	}
}

// Blank lines are the instrumental gaps, and the panel draws a rest on them.
func TestParseLRCKeepsBlankLines(t *testing.T) {
	lines := parseLRC("[00:01.00]One\n[00:20.00]\n[00:30.00]Two\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if lines[1].Text != "" {
		t.Fatalf("want the gap kept as an empty line, got %q", lines[1].Text)
	}
}

func TestParseLRCSortsOutOfOrderStamps(t *testing.T) {
	lines := parseLRC("[00:30.00]Later\n[00:10.00]Earlier\n")
	if lines[0].Text != "Earlier" || lines[1].Text != "Later" {
		t.Fatalf("not sorted: %+v", lines)
	}
}

func TestIndexAtWalksTheLines(t *testing.T) {
	l := &Lyrics{Synced: true, Lines: []Line{
		{At: 5 * time.Second, Text: "a"},
		{At: 10 * time.Second, Text: "b"},
		{At: 20 * time.Second, Text: "c"},
	}}
	cases := []struct {
		pos  time.Duration
		want int
	}{
		{0, -1},
		{4 * time.Second, -1},
		{5 * time.Second, 0},
		{9 * time.Second, 0},
		{10 * time.Second, 1},
		{19*time.Second + 999*time.Millisecond, 1},
		{20 * time.Second, 2},
		{5 * time.Minute, 2},
	}
	for _, c := range cases {
		if got := l.IndexAt(c.pos); got != c.want {
			t.Errorf("at %v: want %d, got %d", c.pos, c.want, got)
		}
	}
}

// Unsynced lyrics have no timeline, so nothing should ever be highlighted.
func TestIndexAtIgnoresUnsynced(t *testing.T) {
	l := &Lyrics{Lines: []Line{{Text: "a"}, {Text: "b"}}}
	if got := l.IndexAt(time.Minute); got != -1 {
		t.Fatalf("want -1 for unsynced lyrics, got %d", got)
	}
}

func TestNormaliseStripsDecoration(t *testing.T) {
	cases := map[string]string{
		"Best Part":                  "best part",
		"Daniel Caesar - Topic":      "daniel caesar",
		"Get You (feat. Kali Uchis)": "get you",
		"Nights - Remastered 2019":   "nights",
		"Don't Know Why!?":           "don t know why",
	}
	for in, want := range cases {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// Searching by title alone returns covers, live cuts and Topic uploads.
// Duration is the strongest signal that two rows are the same recording.
func TestPickPrefersTheMatchingRecording(t *testing.T) {
	recs := []record{
		{TrackName: "Best Part", ArtistName: "Some Cover Band", Duration: 208, PlainLyrics: "x"},
		{TrackName: "Best Part (Live)", ArtistName: "Daniel Caesar", Duration: 260, SyncedLyrics: "[00:01.00]x"},
		{TrackName: "Best Part", ArtistName: "Daniel Caesar", Duration: 210, SyncedLyrics: "[00:01.00]right"},
	}
	got := pick(recs, Query{Title: "Best Part", Artist: "Daniel Caesar", Duration: 209 * time.Second})
	if got == nil || got.SyncedLyrics != "[00:01.00]right" {
		t.Fatalf("picked the wrong record: %+v", got)
	}
}

func TestPickPrefersSyncedOverPlain(t *testing.T) {
	recs := []record{
		{TrackName: "Song", ArtistName: "A", Duration: 200, PlainLyrics: "plain"},
		{TrackName: "Song", ArtistName: "A", Duration: 200, SyncedLyrics: "[00:01.00]synced"},
	}
	got := pick(recs, Query{Title: "Song", Artist: "A", Duration: 200 * time.Second})
	if got == nil || got.SyncedLyrics == "" {
		t.Fatalf("want the synced record, got %+v", got)
	}
}

// Showing the wrong song's words is worse than showing none, so a result that
// matches neither the title nor the length is refused.
func TestPickRefusesAWrongSong(t *testing.T) {
	recs := []record{
		{TrackName: "Something Else Entirely", ArtistName: "Nobody", Duration: 95, SyncedLyrics: "[00:01.00]x"},
	}
	if got := pick(recs, Query{Title: "Best Part", Artist: "Daniel Caesar", Duration: 209 * time.Second}); got != nil {
		t.Fatalf("want no match, got %+v", got)
	}
}

func TestPickSkipsEmptyRecords(t *testing.T) {
	recs := []record{
		{TrackName: "Best Part", ArtistName: "Daniel Caesar", Duration: 210},
	}
	if got := pick(recs, Query{Title: "Best Part", Artist: "Daniel Caesar", Duration: 209 * time.Second}); got != nil {
		t.Fatalf("a record with no words is not a result: %+v", got)
	}
}

func TestConvertPrefersSynced(t *testing.T) {
	l := convert(&record{TrackName: "t", SyncedLyrics: "[00:02.00]hi", PlainLyrics: "hi"})
	if l == nil || !l.Synced || len(l.Lines) != 1 || l.Lines[0].At != 2*time.Second {
		t.Fatalf("got %+v", l)
	}

	l = convert(&record{TrackName: "t", PlainLyrics: "one\ntwo\n"})
	if l == nil || l.Synced || len(l.Lines) != 2 {
		t.Fatalf("plain fallback wrong: %+v", l)
	}

	if l := convert(&record{TrackName: "t", Instrumental: true}); l == nil || !l.Instrumental {
		t.Fatalf("instrumental not carried: %+v", l)
	}
	if l := convert(&record{TrackName: "t"}); l != nil {
		t.Fatalf("a record with nothing in it is not lyrics: %+v", l)
	}
}
