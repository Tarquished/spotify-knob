package osd

import (
	"image"
	"image/color"
	"math"
	"sort"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Ambient mode, the header's beat-echo glow, and the accent's slow journey
// across the cover.
//
// All three are deliberately small: a background treatment, a brightness
// wobble and a colour drift on things that were already there, not new UI
// chrome. Nothing here changes what is drawn where - drawHeader, drawBody
// and drawFooter are untouched - only what sits behind them, how one
// existing glow breathes, and what colour it and everything else that reads
// "accent" happen to be this frame.

// buildAmbientBackground turns a small, already-decoded cover into a soft
// backdrop sized to fill w by h.
//
// The blur is downsample-then-upsample rather than a box blur: scaling the
// cover down to a handful of pixels and back up with CatmullRom interpolation
// produces exactly the soft colour-blob look an ambient backdrop wants, using
// scaling code this file already depends on instead of a second blur
// implementation for RGB. Cheap enough to redo on every art change, so it is
// still only ever done once per track rather than per frame.
func buildAmbientBackground(src *image.RGBA, w, h int) *image.RGBA {
	if src == nil || w <= 0 || h <= 0 {
		return nil
	}
	const tiny = 6
	small := image.NewRGBA(image.Rect(0, 0, tiny, tiny))
	xdraw.CatmullRom.Scale(small, small.Bounds(), src, src.Bounds(), xdraw.Src, nil)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), small, small.Bounds(), xdraw.Src, nil)
	return dst
}

// pulsePeriodFor derives a gentle breathing rate for the header glow from how
// far apart the lyric lines are timed.
//
// This is not a measured tempo. Spotify's audio-features endpoint used to
// carry one; it now refuses requests from an app at this scale (verified: a
// direct call returns 403 for this client), so there is no BPM to read. The
// median gap between lines is an honest stand-in - a slow ballad's lines sit
// further apart than a fast verse's, so the rate still tracks something real
// about the song's pace, just not literally its beat. Median rather than mean
// so one long instrumental gap does not drag the whole estimate off.
//
// Fewer than a few timed gaps, and there is nothing worth deriving a rate
// from at all - the glow stays static rather than guessing.
func pulsePeriodFor(lines []LyricLine) time.Duration {
	var gaps []time.Duration
	for i := 1; i < len(lines); i++ {
		if d := lines[i].At - lines[i-1].At; d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) < 3 {
		return 0
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	median := gaps[len(gaps)/2]

	const minPeriod = 700 * time.Millisecond
	const maxPeriod = 2400 * time.Millisecond
	switch {
	case median < minPeriod:
		return minPeriod
	case median > maxPeriod:
		return maxPeriod
	default:
		return median
	}
}

// pulseBreath is how far into its cycle the glow is at pos, as a 0..1 value
// that eases up and back down rather than sawtoothing - a breath, not a
// blink. Driven by playback position rather than the wall clock, so a paused
// track holds the glow still instead of drifting on regardless.
func pulseBreath(pos time.Duration, period time.Duration) float64 {
	if period <= 0 {
		return 0
	}
	t := float64(pos%period) / float64(period)
	return 0.5 + 0.5*math.Cos(2*math.Pi*t)
}

// liveAccent is the accent colour for this frame: everywhere render() reads
// "accent" - the header bloom, the active line's bar, the footer rail, the
// cover's own placeholder gradient - reads the same one, so the whole panel
// moves together rather than one piece drifting independently of the rest.
//
// Normally that is just w.accent, same as always. Once the track's length is
// known and the cover yielded more than one accent stop, it instead reads a
// position along accentJourney's sweep of the cover: driven by the playhead,
// not the wall clock, so scrubbing to a spot or replaying a section always
// gives back the exact same colour rather than wherever a timer happened to
// be. A song with an unknown duration - or a cover that came back only one
// usable colour - keeps the single static accent it always had.
func (w *LyricsWindow) liveAccent(now time.Time) color.RGBA {
	base := w.accent
	if base.A == 0 {
		base = colAccentFallback
	}
	if w.lastArt == nil || len(w.lastArt.accents) < 2 || w.track.Duration <= 0 {
		return base
	}
	f := clamp01(float64(w.position(now)) / float64(w.track.Duration))
	return accentAlong(w.lastArt.accents, f)
}

// accentAlong maps f in 0..1 onto a position along stops, interpolating
// between whichever two neighbouring stops f falls between so the journey
// reads as continuous drift rather than a handful of visible jump cuts.
func accentAlong(stops []color.RGBA, f float64) color.RGBA {
	n := len(stops)
	switch {
	case n == 0:
		return colAccentFallback
	case n == 1 || f <= 0:
		return stops[0]
	case f >= 1:
		return stops[n-1]
	}
	seg := f * float64(n-1)
	i := int(seg)
	if i >= n-1 {
		i = n - 2
	}
	return lerpColor(stops[i], stops[i+1], seg-float64(i))
}
