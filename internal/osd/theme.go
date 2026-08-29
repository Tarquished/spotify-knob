package osd

import (
	"image/color"
	"math"
)

// Design tokens, written at scale 1 and multiplied by the display scale.
//
// The card is deliberately close in proportion to a media notification: one
// row of artwork, one column of type. Everything else (shadow, halo, accent)
// exists to lift it off whatever is behind it without adding chrome.
const (
	baseCardW  = 392
	baseCardH  = 116
	baseRadius = 24

	// The peek card is the same width but taller: a header plus four rows.
	basePeekH        = 236
	basePeekRowH     = 46
	basePeekThumb    = 34
	basePeekThumbRad = 9
	basePeekTop      = 44 // first row's top edge, below the header
	basePeekRowRad   = 12

	// Canvas padding leaves room for the blurred shadow and the slide-out.
	basePadX      = 44
	basePadTop    = 34
	basePadBottom = 46

	baseArtSize   = 68
	baseArtRadius = 16
	baseInset     = 16
	baseGutter    = 18
	baseRightPad  = 20

	baseShadowBlur   = 22
	baseShadowOffset = 10
	baseSlide        = 18

	// The progress row: a rail thick enough to read at a glance, sitting on
	// its own line under the card's content with a clock either side.
	baseProgressH   = 4
	baseProgressPad = 20 // from the card's side edges to the clocks
	baseProgressGap = 11 // from a clock to the rail
	baseProgressUp  = 16 // rail centre, measured up from the card's bottom
	baseProgressCap = 3.6
)

var (
	// Nearly opaque on purpose. A layered window cannot blur what is behind
	// it, so real translucency just drags whatever is on the desktop through
	// the text. A sliver of it keeps the panel from looking like a sticker.
	colCardTop    = rgba(28, 28, 33, 0.985)
	colCardBottom = rgba(16, 16, 20, 0.985)
	colBorder     = rgba(255, 255, 255, 0.10)
	colShadow     = rgba(0, 0, 0, 0.55)

	colText      = rgba(255, 255, 255, 0.96)
	colTextMuted = rgba(255, 255, 255, 0.54)
	colBarTrack  = rgba(255, 255, 255, 0.13)
	colRowHint   = rgba(255, 255, 255, 0.05)
	colProgress  = rgba(255, 255, 255, 0.12)
	colArtEdge   = rgba(255, 255, 255, 0.10)
	colTopEdge   = rgba(255, 255, 255, 0.11)

	// Spotify green, used until artwork gives us something better.
	colAccentFallback = color.RGBA{R: 30, G: 215, B: 96, A: 255}
)

// layout holds every measurement already multiplied by the display scale.
type layout struct {
	scale  float64
	growUp bool // cards taller than the base grow upward from a fixed bottom

	canvasW, canvasH float64
	cardX, cardY     float64
	cardW, cardH     float64
	peekH            float64
	maxCardH         float64
	radius           float64

	artX, artY  float64
	artSize     float64
	artRadius   float64
	textX       float64
	contentR    float64
	shadowBlur  int
	shadowOff   float64
	slide       float64
	contentW    float64
	barH        float64
	barGap      float64
	barSegments int
	glowRadius  float64
	iconSize    float64
	chipSpacing float64

	peekRowH     float64
	peekThumb    float64
	peekThumbRad float64
	peekTop      float64
	peekRowRad   float64
	progressH    float64
	progressPad  float64
	progressGap  float64
	progressUp   float64
	progressCap  float64
}

func newLayout(scale float64, position string) layout {
	s := func(v float64) float64 { return v * scale }

	l := layout{scale: scale}
	l.growUp = position != "top-center" && position != "top-right"
	l.cardW = s(baseCardW)
	l.cardH = s(baseCardH)
	l.peekH = s(basePeekH)
	l.maxCardH = math.Max(l.cardH, l.peekH)
	l.radius = s(baseRadius)
	l.cardX = s(basePadX)
	l.cardY = s(basePadTop)
	l.canvasW = l.cardW + s(basePadX)*2
	// The canvas is sized for the tallest card so the window never has to be
	// resized mid-animation; shorter cards are aligned inside it.
	l.canvasH = l.maxCardH + s(basePadTop) + s(basePadBottom)

	l.artSize = s(baseArtSize)
	l.artRadius = s(baseArtRadius)
	l.artX = l.cardX + s(baseInset)
	l.artY = l.cardY + s(baseInset)

	l.textX = l.artX + l.artSize + s(baseGutter)
	l.contentR = l.cardX + l.cardW - s(baseRightPad)
	l.contentW = l.contentR - l.textX

	l.shadowBlur = int(s(baseShadowBlur))
	l.shadowOff = s(baseShadowOffset)
	l.slide = s(baseSlide)
	l.barH = s(8)
	l.barGap = s(2.5)
	l.barSegments = 20
	l.glowRadius = s(118)
	l.iconSize = s(10)
	l.chipSpacing = s(7)

	l.peekRowH = s(basePeekRowH)
	l.peekThumb = s(basePeekThumb)
	l.peekThumbRad = s(basePeekThumbRad)
	l.peekTop = s(basePeekTop)
	l.peekRowRad = s(basePeekRowRad)
	l.progressH = s(baseProgressH)
	l.progressPad = s(baseProgressPad)
	l.progressGap = s(baseProgressGap)
	l.progressUp = s(baseProgressUp)
	l.progressCap = s(baseProgressCap)
	return l
}

// cardTop is where a card of the given height starts inside the canvas.
// Cards grow away from the screen edge they sit against.
func (l layout) cardTop(h float64) float64 {
	if l.growUp {
		return l.cardY + (l.maxCardH - h)
	}
	return l.cardY
}

// cardBottom is the fixed edge the window is positioned against.
func (l layout) cardBottom() float64 { return l.cardY + l.maxCardH }

func (l layout) px(v float64) float64 { return v * l.scale }
