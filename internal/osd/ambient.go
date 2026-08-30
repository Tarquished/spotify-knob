package osd

import (
	"image"
	"math"
	"sort"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Ambient mode and the header's beat-echo glow.
//
// Both are deliberately small: a background treatment and a brightness
// wobble on something that was already there, not new UI chrome. Nothing
// here changes what is drawn where - drawHeader, drawBody and drawFooter are
// untouched - only what sits behind them and how one existing glow breathes.

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
