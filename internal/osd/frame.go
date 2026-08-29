package osd

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"time"
)

// Kind selects which card is drawn.
type Kind int

const (
	KindVolume Kind = iota
	KindTrack
	KindPeek
	// KindNotice says one short thing about the track - "no lyrics for this
	// one" - and leaves. It borrows the track card's shape so a message and a
	// skip look like the same object.
	KindNotice
)

// Direction labels a track change.
type Direction int

const (
	Forward Direction = iota
	Backward
)

func (d Direction) label() string {
	if d == Backward {
		return "PREVIOUS"
	}
	return "NEXT"
}

// frameState is the card's content. It deliberately holds nothing about the
// entry animation: the card is always composed at rest, and the slide and
// fade are applied when the frame is handed to Windows. That keeps the
// expensive part cacheable across every frame where only the animation moves.
type frameState struct {
	kind     Kind
	dir      Direction
	volume   int     // 0-100, the number shown
	bar      float64 // 0-1, animated separately so it glides
	title    string
	artist   string
	label    string // KindNotice: the chip above the message
	art      *artwork
	accent   color.RGBA
	artFade  float64 // 0-1 fade for artwork that arrived late
	pending  bool    // track details still loading
	progress float64 // 0-1 through the current track
	elapsed  time.Duration
	total    time.Duration

	queue     []Track    // upcoming tracks, for the peek card
	queueArt  []*artwork // resolved covers, same length as queue
	selected  int        // highlighted row in the peek card
	artLoaded int        // how many row covers have arrived, for the frame cache
}

// contentKey identifies a rendered card, so an unchanged one is not redrawn.
type contentKey struct {
	kind      Kind
	dir       Direction
	volume    int
	bar       int // quantised: sub-pixel bar changes are invisible
	title     string
	artist    string
	label     string
	art       *artwork
	accent    color.RGBA
	artFade   int
	pending   bool
	progress  int
	elapsed   int // whole seconds; the clock cannot show more
	total     int
	queueRev  int
	selected  int
	artLoaded int
}

func (st frameState) key(barPx, cardPx float64, rev int) contentKey {
	return contentKey{
		kind:    st.kind,
		dir:     st.dir,
		volume:  st.volume,
		bar:     int(st.bar * barPx * 2), // half-pixel steps along the bar
		title:   st.title,
		artist:  st.artist,
		label:   st.label,
		art:     st.art,
		accent:  st.accent,
		artFade: int(st.artFade * 64),
		pending: st.pending,
		// Quantised to pixels along the card: a three-minute track only
		// crosses a pixel about twice a second, so the frame cache holds.
		progress:  int(st.progress * cardPx),
		elapsed:   int(st.elapsed / time.Second),
		total:     int(st.total / time.Second),
		queueRev:  rev,
		selected:  st.selected,
		artLoaded: st.artLoaded,
	}
}

// renderFrame composes the card at rest.
func (o *OSD) renderFrame(st frameState) {
	l := o.layout
	p := o.paint
	p.clear()

	accent := st.accent
	if accent.A == 0 {
		accent = colAccentFallback
	}

	cw, rad := l.cardW, l.radius
	ch := l.cardH
	if st.kind == KindPeek {
		ch = l.peekH
	}
	cx := l.cardX
	cy := l.cardTop(ch)

	// Shadow first, so everything else sits on top of it. It is cached: the
	// geometry never moves now that the slide happens at present time.
	p.blitMask(o.shadowMask(cx, cy+l.shadowOff, cw, ch, rad), colShadow)

	// Card body: a vertical graphite gradient, not flat black, so the surface
	// reads as a physical panel.
	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.linear(cx, cy, cx, cy+ch, colCardTop, colCardBottom)

	// Accent halo bleeding out of the artwork, clipped to the card by
	// painting it through the same rounded path.
	haloX, haloY, haloR := l.artX+l.artSize/2, cy+l.px(baseInset)+l.artSize/2, l.glowRadius
	if st.kind == KindPeek {
		// No single cover to anchor on, so the glow just warms the top-left
		// corner behind the header.
		haloX, haloY, haloR = cx+cw*0.22, cy+l.px(20), l.glowRadius*1.5
	}
	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.radial(haloX, haloY, haloR, scaleAlpha(premul(accent), 0.24))

	// Hairline border separates the card from bright backgrounds.
	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.roundRectRev(cx+1, cy+1, cw-2, ch-2, rad-1)
	p.flat(colBorder)

	// A brighter sliver along the top edge only. It is the detail that makes
	// the panel read as a lit surface rather than a flat swatch.
	edge := l.px(1.2)
	p.begin(cx, cy, cw, edge*2)
	p.roundRect(cx, cy, cw, ch, rad)
	p.roundRectRev(cx, cy+edge, cw, ch-edge, rad-edge)
	p.flat(colTopEdge)

	if st.kind != KindPeek {
		o.drawArtwork(st, accent, l.artX, cy+l.px(baseInset))
	}

	switch st.kind {
	case KindVolume:
		o.drawVolume(st, accent, cy)
		o.drawProgress(st, accent, cx, cy, cw, ch)
	case KindTrack:
		o.drawTrack(st, accent, cy)
		o.drawProgress(st, accent, cx, cy, cw, ch)
	case KindNotice:
		o.drawNotice(st, accent, cy)
		o.drawProgress(st, accent, cx, cy, cw, ch)
	case KindPeek:
		o.drawPeek(st, accent, cx, cy, cw)
	}
}

// drawProgress is the footer status row: where the playhead sits in the
// current track, with the elapsed and total time either side of it.
//
// It used to be a two-pixel hairline hugging the card's bottom curve. That was
// accurate but nearly invisible, so this is a proper row instead - a thicker
// rail with a bright cap at the playhead, on its own line under the content.
// It still cannot be confused with the volume meter above it: that one is
// segmented, sits in the text column, and has no cap.
func (o *OSD) drawProgress(st frameState, accent color.RGBA, cx, cy, cw, ch float64) {
	l := o.layout
	p := o.paint
	face := o.fonts.face(regular, l.px(10.5))

	// The clock column is at least as wide as a four-character time even when
	// there is nothing to put in it, so the rail's ends never move: not as the
	// digits tick over, and not when a track's length finally arrives.
	known := st.total > 0
	colW := measure(face, "0:00")
	midY := cy + ch - l.progressUp

	if known {
		elapsed, total := formatClock(st.elapsed), formatClock(st.total)
		totalW := measure(face, total)
		colW = math.Max(colW, math.Max(measure(face, elapsed), totalW))

		baseline := midY + l.px(3.8)
		drawText(p.dst, face, colTextMuted, cx+l.progressPad, baseline, elapsed)
		drawText(p.dst, face, colTextMuted, cx+cw-l.progressPad-totalW, baseline, total)
	}

	h := l.progressH
	rad := h / 2
	x := cx + l.progressPad + colW + l.progressGap
	w := cx + cw - l.progressPad - colW - l.progressGap - x
	y := midY - h/2
	if w <= h {
		return
	}

	p.begin(x, y, w, h)
	p.roundRect(x, y, w, h, rad)
	p.flat(colProgress)

	if !known {
		// An empty rail, which is the truth: Spotify has not told us how long
		// this is. Better than a blank strip where the row should be.
		return
	}

	// The fill never gets shorter than its own end caps, so a track that has
	// just started still reads as a dot at the left rather than a sliver.
	fw := math.Max(w*clamp01(st.progress), h)
	p.begin(x, y, fw, h)
	p.roundRect(x, y, fw, h, rad)
	p.linear(x, 0, x+w, 0, scaleAlpha(premul(accent), 0.7), premul(lighten(accent, 0.25)))

	// The cap is what makes this read as a playhead. A soft accent glow under
	// a small white disc: the only pure white on the card, so the eye finds
	// the position before it reads either clock.
	capR := l.progressCap
	capX := math.Min(math.Max(x+fw, x+capR), x+w-capR)
	glowR := capR * 2.8
	p.begin(capX-glowR, midY-glowR, glowR*2, glowR*2)
	p.circle(capX, midY, glowR)
	p.radial(capX, midY, glowR, scaleAlpha(premul(accent), 0.55))

	p.begin(capX-capR, midY-capR, capR*2, capR*2)
	p.circle(capX, midY, capR)
	p.flat(colText)
}

// formatClock renders a playhead position the way a player does.
func formatClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int(d.Round(time.Second) / time.Second)
	if h := sec / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// drawPeek is the queue browser: the tracks coming up, with the selected one
// highlighted. Turning the knob moves the highlight; pressing it plays.
func (o *OSD) drawPeek(st frameState, accent color.RGBA, cx, cy, cw float64) {
	l := o.layout
	p := o.paint
	fs := o.fonts

	headFace := fs.face(bold, l.px(9.5))
	titleFace := fs.face(semibold, l.px(13))
	artistFace := fs.face(regular, l.px(11))

	drawTracked(p.dst, headFace, premul(accent), cx+l.px(22), cy+l.px(30), "UP NEXT", l.px(1.1))

	if len(st.queue) == 0 {
		drawText(p.dst, titleFace, colTextMuted, cx+l.px(22), cy+l.px(70), "Queue is empty")
		return
	}

	textX := cx + l.px(22) + l.peekThumb + l.px(14)
	textMax := cx + cw - l.px(22) - textX

	rows := st.queue
	if len(rows) > peekRows {
		rows = rows[:peekRows] // never draw past the card
	}
	for i, t := range rows {
		ry := cy + l.peekTop + float64(i)*l.peekRowH
		selected := i == st.selected

		if selected {
			p.begin(cx+l.px(12), ry+l.px(2), cw-l.px(24), l.peekRowH-l.px(4))
			p.roundRect(cx+l.px(12), ry+l.px(2), cw-l.px(24), l.peekRowH-l.px(4), l.peekRowRad)
			p.flat(scaleAlpha(premul(accent), 0.18))
		}

		var art *artwork
		if i < len(st.queueArt) {
			art = st.queueArt[i]
		}
		o.drawThumb(art, accent, cx+l.px(22), ry+(l.peekRowH-l.peekThumb)/2)

		title := colText
		if !selected {
			title = rgba(255, 255, 255, 0.78)
		}
		drawText(p.dst, titleFace, title, textX, ry+l.px(21),
			truncate(titleFace, t.Title, textMax))
		drawText(p.dst, artistFace, colTextMuted, textX, ry+l.px(36),
			truncate(artistFace, t.Artist, textMax))
	}
}

// drawThumb is the small square cover used in the queue rows.
func (o *OSD) drawThumb(art *artwork, accent color.RGBA, x, y float64) {
	l := o.layout
	p := o.paint
	size := l.peekThumb
	rad := l.peekThumbRad

	clip := func() {
		p.begin(x, y, size, size)
		p.roundRect(x, y, size, size, rad)
	}

	if art != nil && art.thumb != nil {
		clip()
		p.picture(art.thumb, x, y, 1)
	} else {
		clip()
		p.linear(x, y, x+size, y+size,
			scaleAlpha(premul(accent), 0.34), scaleAlpha(premul(accent), 0.14))
	}

	p.begin(x, y, size, size)
	p.roundRect(x, y, size, size, rad)
	p.roundRectRev(x+1, y+1, size-2, size-2, rad-1)
	p.flat(colArtEdge)
}

// drawArtwork paints the cover, or a record-shaped placeholder in its accent
// colour while the image is still on the way.
func (o *OSD) drawArtwork(st frameState, accent color.RGBA, x, y float64) {
	l := o.layout
	p := o.paint
	size := l.artSize

	clip := func() {
		p.begin(x, y, size, size)
		p.roundRect(x, y, size, size, l.artRadius)
	}

	if st.art != nil && st.art.img != nil && st.artFade > 0 {
		// Base plate first: while the cover fades in, it fades in over
		// something, not over the card gradient.
		clip()
		p.linear(x, y, x+size, y+size,
			scaleAlpha(premul(accent), 0.30), scaleAlpha(premul(accent), 0.12))
		clip()
		p.picture(st.art.img, x, y, st.artFade)
		o.artEdge(x, y, size)
		return
	}

	// Placeholder: accent plate with a vinyl motif.
	clip()
	p.linear(x, y, x+size, y+size,
		scaleAlpha(premul(accent), 0.42), scaleAlpha(premul(accent), 0.16))

	r := size * 0.27
	p.begin(x+size/2-r, y+size/2-r, r*2, r*2)
	p.circle(x+size/2, y+size/2, r)
	p.flat(rgba(255, 255, 255, 0.22))

	hr := size * 0.085
	p.begin(x+size/2-hr, y+size/2-hr, hr*2, hr*2)
	p.circle(x+size/2, y+size/2, hr)
	p.flat(colCardBottom)

	o.artEdge(x, y, size)
}

// artEdge outlines the cover so a dark album does not dissolve into the card.
func (o *OSD) artEdge(x, y, size float64) {
	p := o.paint
	rad := o.layout.artRadius
	p.begin(x, y, size, size)
	p.roundRect(x, y, size, size, rad)
	p.roundRectRev(x+1, y+1, size-2, size-2, rad-1)
	p.flat(colArtEdge)
}

func (o *OSD) drawVolume(st frameState, accent color.RGBA, cy float64) {
	l := o.layout
	p := o.paint
	fs := o.fonts

	pctFace := fs.face(semibold, l.px(21))
	titleFace := fs.face(semibold, l.px(14.5))
	artistFace := fs.face(regular, l.px(12))

	pct := fmt.Sprintf("%d%%", st.volume)
	pctW := measure(pctFace, pct)
	baseline := cy + l.px(43)

	// Percentage is the anchor: right-aligned, sharing a baseline with the
	// track title so the row reads as one line.
	drawText(p.dst, pctFace, colText, l.contentR-pctW, baseline, pct)

	title := st.title
	if title == "" {
		title = "Spotify"
	}
	titleMax := l.contentW - pctW - l.px(14)
	drawText(p.dst, titleFace, colText, l.textX, baseline, truncate(titleFace, title, titleMax))

	if st.artist != "" {
		drawText(p.dst, artistFace, colTextMuted, l.textX, cy+l.px(63),
			truncate(artistFace, st.artist, l.contentW))
	}

	barY := cy + l.px(77)
	barW := l.contentW

	// A segmented meter, not another bar. One segment per knob click, which
	// both ties the reading to the physical control and makes it impossible
	// to confuse with the continuous progress hairline along the card edge.
	n := l.barSegments
	segW := (barW - l.barGap*float64(n-1)) / float64(n)
	segRad := l.px(1.5)
	lit := int(clamp01(st.bar)*float64(n) + 0.5)
	if st.volume > 0 && lit == 0 {
		lit = 1 // never let a non-zero volume read as empty
	}

	// One soft bloom under the lit run, rather than per segment.
	if lit > 0 {
		litW := float64(lit)*(segW+l.barGap) - l.barGap
		p.blitMask(o.barGlowMask(l.textX, barY, litW, l.barH, l.barH/2),
			scaleAlpha(premul(accent), 0.4))
	}

	for i := 0; i < n; i++ {
		x := l.textX + float64(i)*(segW+l.barGap)
		p.begin(x, barY, segW, l.barH)
		p.roundRect(x, barY, segW, l.barH, segRad)
		if i < lit {
			// Gradient across the whole run, so the meter reads as one object.
			p.linear(l.textX, 0, l.textX+barW, 0,
				premul(accent), premul(lighten(accent, 0.3)))
		} else {
			p.flat(colBarTrack)
		}
	}
}

func (o *OSD) drawTrack(st frameState, accent color.RGBA, cy float64) {
	l := o.layout
	p := o.paint
	fs := o.fonts

	chipFace := fs.face(bold, l.px(9.5))
	titleFace := fs.face(semibold, l.px(15))
	artistFace := fs.face(regular, l.px(12.5))

	// Direction chip: icon plus a tracked-out label, both in the accent.
	iconY := cy + l.px(25.5)
	iconW := l.iconSize * iconAspect
	p.begin(l.textX, iconY, iconW, l.iconSize)
	skipIcon(p, l.textX, iconY, l.iconSize, st.dir == Backward)
	p.flat(premul(accent))

	labelX := l.textX + iconW + l.chipSpacing
	drawTracked(p.dst, chipFace, premul(accent), labelX, cy+l.px(34), st.dir.label(), l.px(1.1))

	if st.pending {
		// Skeleton rows while the new track is still being read back.
		o.skeleton(l.textX, cy+l.px(46), l.contentW*0.72, l.px(13))
		o.skeleton(l.textX, cy+l.px(66), l.contentW*0.45, l.px(11))
		return
	}

	title := st.title
	if title == "" {
		title = "Unknown track"
	}
	drawText(p.dst, titleFace, colText, l.textX, cy+l.px(58),
		truncate(titleFace, title, l.contentW))

	if st.artist != "" {
		drawText(p.dst, artistFace, colTextMuted, l.textX, cy+l.px(78),
			truncate(artistFace, st.artist, l.contentW))
	}
}

// drawNotice is the track card with a worded chip instead of a skip glyph.
func (o *OSD) drawNotice(st frameState, accent color.RGBA, cy float64) {
	l := o.layout
	p := o.paint
	fs := o.fonts

	chipFace := fs.face(bold, l.px(9.5))
	titleFace := fs.face(semibold, l.px(14))
	artistFace := fs.face(regular, l.px(12.5))

	drawTracked(p.dst, chipFace, premul(accent), l.textX, cy+l.px(34), st.label, l.px(1.1))

	drawText(p.dst, titleFace, colText, l.textX, cy+l.px(58),
		truncate(titleFace, st.title, l.contentW))
	if st.artist != "" {
		drawText(p.dst, artistFace, colTextMuted, l.textX, cy+l.px(78),
			truncate(artistFace, st.artist, l.contentW))
	}
}

func (o *OSD) skeleton(x, y, w, h float64) {
	p := o.paint
	p.begin(x, y, w, h)
	p.roundRect(x, y, w, h, h/2)
	p.flat(rgba(255, 255, 255, 0.09))
}

// peekRows is how many queue entries fit on the card.
const peekRows = 4

// iconAspect is the skip glyph's width as a multiple of its height.
const iconAspect = 1.19

// skipIcon draws the double-triangle skip glyph, mirrored for "previous".
// The two triangles are kept apart rather than overlapping, which is what
// stops them merging into a blob at the size this renders (10px).
func skipIcon(p *painter, x, y, s float64, back bool) {
	w := s * iconAspect
	mx := func(v float64) float64 {
		if back {
			return x + w - (v - x)
		}
		return v
	}
	tri := func(x0 float64) {
		p.polygon(
			mx(x0), y,
			mx(x0+s*0.44), y+s/2,
			mx(x0), y+s,
		)
	}
	tri(x)
	tri(x + s*0.50)

	bw := s * 0.17
	barX := x + s*1.02
	if back {
		barX = mx(barX + bw)
	}
	p.roundRect(barX, y, bw, s, bw*0.35)
}

// unusedShadowRect is the area the blurred shadow can reach.
func (l layout) shadowRect() image.Rectangle {
	spread := l.shadowBlur*3 + 2
	return image.Rect(
		int(l.cardX)-spread,
		int(l.cardY+l.shadowOff)-spread,
		int(l.cardX+l.cardW)+spread,
		int(l.cardY+l.shadowOff+l.cardH)+spread,
	)
}
