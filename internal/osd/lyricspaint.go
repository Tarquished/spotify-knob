package osd

import (
	"fmt"
	"image/color"
	"math"
	"time"
)

// Painting and input for the lyrics panel.
//
// The visual language is deliberately the overlay card's: same graphite
// gradient, same hairline border, same accent taken from the album cover,
// same progress rail. The panel should read as the same product seen from
// closer up, not as a second app.

var (
	colLyrPast   = rgba(255, 255, 255, 0.26)
	colLyrNext   = rgba(255, 255, 255, 0.50)
	colLyrActive = rgba(255, 255, 255, 0.97)
	colLyrPlain  = rgba(255, 255, 255, 0.74)
	colLyrDivide = rgba(255, 255, 255, 0.07)
	colLyrGrip   = rgba(255, 255, 255, 0.22)
	colLyrClose  = rgba(255, 255, 255, 0.45)
)

func (w *LyricsWindow) render() {
	p := w.paint
	p.clearRect(w.win.w, w.win.h)

	W, H := float64(w.win.w), float64(w.win.h)
	m := w.px(lyrMargin)
	cx, cy, cw, ch := m, m, W-2*m, H-2*m
	rad := w.px(lyrRadius)

	accent := w.accent
	if accent.A == 0 {
		accent = colAccentFallback
	}

	// A soft ring instead of a blurred shadow. The panel is resizable, and a
	// real drop shadow would mean re-blurring a 900x1100 mask on every frame
	// of a corner drag; three nested strokes cost nothing and read as depth
	// against both bright and dark backgrounds.
	for i := 3; i >= 1; i-- {
		f := float64(i)
		p.begin(cx-f, cy-f, cw+2*f, ch+2*f)
		p.roundRect(cx-f, cy-f, cw+2*f, ch+2*f, rad+f)
		p.roundRectRev(cx-f+1, cy-f+1, cw+2*f-2, ch+2*f-2, rad+f-1)
		p.flat(rgba(0, 0, 0, 0.34/f))
	}

	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.linear(cx, cy, cx, cy+ch, colCardTop, colCardBottom)

	// Accent bloom behind the header, anchored on the cover.
	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.radial(cx+w.px(48), cy+w.px(30), w.px(240), scaleAlpha(premul(accent), 0.20))

	p.begin(cx, cy, cw, ch)
	p.roundRect(cx, cy, cw, ch, rad)
	p.roundRectRev(cx+1, cy+1, cw-2, ch-2, rad-1)
	p.flat(colBorder)

	edge := w.px(1.2)
	p.begin(cx, cy, cw, edge*2)
	p.roundRect(cx, cy, cw, ch, rad)
	p.roundRectRev(cx, cy+edge, cw, ch-edge, rad-edge)
	p.flat(colTopEdge)

	w.drawHeader(cx, cy, cw, accent)
	w.drawBody(accent)
	w.drawFooter(cx, cy+ch, cw, accent)
	w.drawGrip(cx+cw, cy+ch)
}

// headerMetrics places the header's two controls. Like footerMetrics, it is
// shared by the painter and the hit test so what is drawn and what is
// clickable can never drift apart.
func (w *LyricsWindow) headerMetrics() (closeX, closeY, closeR, openX, sliderX, sliderW, sliderY float64) {
	m := w.px(lyrMargin)
	cx, cw := m, float64(w.win.w)-2*m
	inset := w.px(18)

	closeR = w.px(11)
	closeX = cx + cw - inset - closeR
	closeY = m + w.px(19) + closeR

	// The "open in Spotify" icon sits on the close button's own row, the
	// same size and the same hover treatment - one small icon toolbar,
	// rather than a second control competing with the title underneath it.
	openX = closeX - closeR*2 - w.px(6)

	sliderW = w.px(lyrSliderW)
	sliderX = cx + cw - inset - sliderW
	sliderY = m + w.px(58)
	return closeX, closeY, closeR, openX, sliderX, sliderW, sliderY
}

func (w *LyricsWindow) drawHeader(cx, cy, cw float64, accent color.RGBA) {
	p := w.paint
	fs := w.fonts
	inset := w.px(18)
	thumb := w.px(lyrThumb)

	w.drawCover(cx+inset, cy+w.px(13), thumb, accent)

	titleFace := fs.face(semibold, w.px(14.5))
	artistFace := fs.face(regular, w.px(12))
	badgeFace := fs.face(bold, w.px(8))

	textX := cx + inset + thumb + w.px(14)
	closeX, closeY, closeR, openX, sliderX, sliderW, sliderY := w.headerMetrics()
	showOpen := w.track.URI != ""

	// The badge normally names where the words came from. It gives way to the
	// opacity readout while the slider drags, and to a hint of what the icon
	// beside it does while that icon is hovered - the same slot doing three
	// jobs rather than three separate labels competing for room.
	label, labelCol := w.sourceLabel(), rgba(255, 255, 255, 0.30)
	switch {
	case w.sliding || time.Now().Before(w.sliderShow):
		label = fmt.Sprintf("%d%%", int(w.opacity()*100+0.5))
		labelCol = premul(accent)
	case w.openHot && showOpen:
		label = "OPEN IN SPOTIFY"
		labelCol = premul(accent)
	}

	// Everything in this row reads right-to-left off the icon toolbar: the
	// open icon (when there is a track to open) sits left of close, and the
	// badge sits left of whichever of those is leftmost.
	rightEdge := closeX - closeR
	if showOpen {
		rightEdge = openX - closeR
	}
	badgeW := 0.0
	if label != "" {
		badgeW = measureTracked(badgeFace, label, w.px(0.9)) + w.px(14)
		drawTracked(p.dst, badgeFace, labelCol,
			rightEdge-badgeW, cy+w.px(34), label, w.px(0.9))
	}

	textMax := rightEdge - badgeW - w.px(12) - textX
	title := w.track.Title
	if title == "" {
		title = "Nothing playing"
	}
	drawText(p.dst, titleFace, colText, textX, cy+w.px(29),
		truncate(titleFace, title, textMax))
	if w.track.Artist != "" {
		drawText(p.dst, artistFace, colTextMuted, textX, cy+w.px(48),
			truncate(artistFace, w.track.Artist, textMax))
	}

	w.drawClose(closeX, closeY, closeR)
	if showOpen {
		w.drawOpenIcon(openX, closeY, closeR, accent)
	}
	w.drawOpacitySlider(sliderX, sliderY, sliderW, accent)

	div := cy + w.px(lyrHeaderH) - w.px(1)
	p.begin(cx+inset, div, cw-2*inset, 1)
	p.roundRect(cx+inset, div, cw-2*inset, 1, 0.5)
	p.flat(colLyrDivide)
}

// drawOpenIcon is the "open in Spotify" control: a small icon button on the
// close button's own row, same size and same hover-fills-the-circle
// treatment. The first version of this was a labelled pill under the artist
// name; it was the one thing in the panel that looked like a UI control
// bolted on rather than belonging to it, so it is an icon now, the way close
// and the opacity slider already are.
func (w *LyricsWindow) drawOpenIcon(cx, cy, r float64, accent color.RGBA) {
	p := w.paint
	col := colLyrClose
	if w.openHot {
		p.begin(cx-r, cy-r, r*2, r*2)
		p.circle(cx, cy, r)
		p.flat(rgba(255, 255, 255, 0.12))
		col = premul(accent)
	}
	drawArrowGlyph(p, cx, cy, r*0.62, col)
}

// drawArrowGlyph draws a small diagonal arrow, built from two vector strokes
// rather than a font glyph. The first attempt at this icon used a Unicode
// arrow character as text; it rendered as a tofu box, because nothing
// guarantees the loaded face carries that codepoint. A shape built from the
// same polygon primitives the rest of the panel already uses has no such
// failure mode.
//
// The arrow is built pointing along +x in its own local space, tip at the
// origin, then rotated -45 degrees into place - simpler to get symmetric
// than reasoning about the diagonal directly.
func drawArrowGlyph(p *painter, cx, cy, r float64, col color.RGBA) {
	const c45 = 0.70710678
	rot := func(x, y float64) (float64, float64) {
		return cx + c45*(x+y), cy + c45*(y-x)
	}

	shaftLen := r * 0.95
	headLen := r * 0.66
	headHalf := r * 0.5
	half := math.Max(r*0.16, 1)

	p.begin(cx-r, cy-r, r*2, r*2)
	x0, y0 := rot(-shaftLen, -half)
	x1, y1 := rot(-shaftLen, half)
	x2, y2 := rot(shaftLen-headLen, half)
	x3, y3 := rot(shaftLen-headLen, -half)
	p.polygon(x0, y0, x1, y1, x2, y2, x3, y3)
	p.flat(col)

	p.begin(cx-r, cy-r, r*2, r*2)
	tx, ty := rot(shaftLen, 0)
	bx1, by1 := rot(shaftLen-headLen, headHalf)
	bx2, by2 := rot(shaftLen-headLen, -headHalf)
	p.polygon(tx, ty, bx1, by1, bx2, by2)
	p.flat(col)
}

// drawOpacitySlider is the transparency control: a half-filled disc for a
// label, then a track whose filled part is how solid the panel is.
//
// It is drawn at the panel's own opacity like everything else, which is the
// point - the control shows you the setting by being subject to it.
func (w *LyricsWindow) drawOpacitySlider(x, midY, ww float64, accent color.RGBA) {
	p := w.paint
	icon := w.px(4.6)
	iconX := x - w.px(9)

	// The glyph: a ring, with its left half filled. Nothing in the font set
	// draws this, and at 9px an icon reads faster than a word.
	p.begin(iconX-icon, midY-icon, icon*2, icon*2)
	p.circle(iconX, midY, icon)
	p.circleRev(iconX, midY, icon-math.Max(w.px(1), 1))
	p.flat(rgba(255, 255, 255, 0.34))

	p.begin(iconX-icon, midY-icon, icon, icon*2)
	p.circle(iconX, midY, icon-math.Max(w.px(1), 1))
	p.flat(rgba(255, 255, 255, 0.34))

	h := w.px(lyrSliderH)
	if w.sliderHot || w.sliding {
		h = w.px(4.5)
	}
	y := midY - h/2
	p.begin(x, y, ww, h)
	p.roundRect(x, y, ww, h, h/2)
	p.flat(colProgress)

	f := opacityFraction(w.opts.Opacity)
	fw := math.Max(ww*f, h)
	p.begin(x, y, fw, h)
	p.roundRect(x, y, fw, h, h/2)
	p.flat(scaleAlpha(premul(accent), 0.85))

	knob := w.px(lyrSliderKnob)
	if w.sliderHot || w.sliding {
		knob = w.px(6.5)
	}
	kx := math.Min(math.Max(x+ww*f, x+knob), x+ww-knob)
	p.begin(kx-knob, midY-knob, knob*2, knob*2)
	p.circle(kx, midY, knob)
	p.flat(colText)
}

// sourceLabel names the provider and whether the timing is real.
func (w *LyricsWindow) sourceLabel() string {
	if w.doc == nil || w.state != docReady {
		return ""
	}
	if w.doc.Instrumental {
		return w.doc.Source
	}
	if w.doc.Synced {
		return w.doc.Source + " · SYNCED"
	}
	return w.doc.Source + " · TEXT"
}

func (w *LyricsWindow) drawCover(x, y, size float64, accent color.RGBA) {
	p := w.paint
	rad := size * 0.22

	clip := func() {
		p.begin(x, y, size, size)
		p.roundRect(x, y, size, size, rad)
	}
	if w.lastArt != nil && w.lastArt.img != nil {
		clip()
		p.picture(w.lastArt.img, x, y, 1)
	} else {
		clip()
		p.linear(x, y, x+size, y+size,
			scaleAlpha(premul(accent), 0.40), scaleAlpha(premul(accent), 0.15))
		r := size * 0.26
		p.begin(x+size/2-r, y+size/2-r, r*2, r*2)
		p.circle(x+size/2, y+size/2, r)
		p.flat(rgba(255, 255, 255, 0.20))
	}
	p.begin(x, y, size, size)
	p.roundRect(x, y, size, size, rad)
	p.roundRectRev(x+1, y+1, size-2, size-2, rad-1)
	p.flat(colArtEdge)
}

// drawClose is the dismiss button: a ring that fills in on hover, with an X.
func (w *LyricsWindow) drawClose(cx, cy, r float64) {
	p := w.paint
	col := colLyrClose
	if w.closeHot {
		p.begin(cx-r, cy-r, r*2, r*2)
		p.circle(cx, cy, r)
		p.flat(rgba(255, 255, 255, 0.12))
		col = colText
	}

	// Two bars crossed at 45 degrees, drawn as quads so they stay crisp at
	// the size this renders.
	arm := r * 0.46
	half := math.Max(w.px(0.85), 1)
	for _, sign := range []float64{1, -1} {
		dx, dy := arm/math.Sqrt2, sign*arm/math.Sqrt2
		nx, ny := -dy/arm*half, dx/arm*half
		p.begin(cx-r, cy-r, r*2, r*2)
		p.polygon(
			cx-dx+nx, cy-dy+ny,
			cx+dx+nx, cy+dy+ny,
			cx+dx-nx, cy+dy-ny,
			cx-dx-nx, cy-dy-ny,
		)
		p.flat(col)
	}
}

func (w *LyricsWindow) drawBody(accent color.RGBA) {
	bx, by, bw, bh := w.bodyRect()
	if bw <= 0 || bh <= 0 {
		return
	}
	if w.state != docReady || w.doc == nil || len(w.para) == 0 {
		w.drawPlaceholder(bx, by, bw, bh, accent)
		return
	}
	if w.doc.Instrumental {
		w.drawCentred(bx, by, bw, bh, "Instrumental", "No words in this one")
		return
	}

	clip := clipTo(w.paint.dst, bx, by, bw, bh)
	if clip == nil {
		return
	}
	face := w.fonts.face(semibold, w.px(lyrLineSize))
	fade := w.lineH * lyrFadeLines
	synced := w.doc.Synced

	for i := range w.para {
		para := &w.para[i]
		top := by - w.scroll + para.top
		if top+para.h < by-w.lineH || top > by+bh+w.lineH {
			continue // off the visible strip
		}

		alpha := 1.0
		base := colLyrPlain
		if synced {
			switch {
			case i == w.active:
				base = colLyrActive
			case i < w.active:
				base = colLyrPast
			default:
				base = colLyrNext
			}
		}
		// Fade toward both edges so lines dissolve instead of being cut.
		mid := top + para.h/2
		if d := mid - by; d < fade {
			alpha *= clamp01(d / fade)
		}
		if d := by + bh - mid; d < fade {
			alpha *= clamp01(d / fade)
		}
		if alpha <= 0.01 {
			continue
		}

		if para.blank {
			if synced && i == w.active {
				w.drawRest(bx, top+para.h/2, accent, alpha)
			}
			continue
		}

		if synced && i == w.active {
			// A short accent bar in the left gutter, the one piece of colour
			// in the body. It marks the line without recolouring the words.
			barW := w.px(lyrActiveBarW)
			barX := bx - w.px(15)
			p := w.paint
			p.begin(barX, top+w.px(3), barW, para.h-w.px(6))
			p.roundRect(barX, top+w.px(3), barW, para.h-w.px(6), barW/2)
			p.flat(scaleAlpha(premul(accent), alpha*0.95))
		}

		col := scaleAlpha(base, alpha)
		for r, row := range para.rows {
			baseline := top + w.lineH*0.74 + float64(r)*w.lineH
			drawText(clip, face, col, bx, baseline, row)
		}
	}
}

// drawRest is the three-dot marker shown during an instrumental gap, pulsing
// so a long break does not look like the panel has frozen.
func (w *LyricsWindow) drawRest(x, y float64, accent color.RGBA, alpha float64) {
	p := w.paint
	r := w.px(3)
	gap := w.px(11)
	phase := float64(time.Now().UnixMilli()%1800) / 1800
	for i := 0; i < 3; i++ {
		// Each dot peaks a third of a cycle after the one before it.
		t := math.Mod(phase-float64(i)*0.18+1, 1)
		lift := 0.45 + 0.55*math.Max(0, math.Cos(2*math.Pi*t))
		cx := x + float64(i)*gap + r
		p.begin(cx-r, y-r, r*2, r*2)
		p.circle(cx, y, r)
		p.flat(scaleAlpha(premul(accent), alpha*lift))
	}
}

func (w *LyricsWindow) drawPlaceholder(bx, by, bw, bh float64, accent color.RGBA) {
	switch w.state {
	case docLoading:
		w.drawLoading(bx, by, bw, bh, accent)
	case docMissing:
		w.drawCentred(bx, by, bw, bh, "No lyrics for this track",
			"LRCLIB has nothing filed for this recording")
	case docFailed:
		w.drawCentred(bx, by, bw, bh, "Could not reach LRCLIB",
			"Check the connection and press Ctrl + knob again")
	default:
		w.drawCentred(bx, by, bw, bh, "Nothing playing", "")
	}
}

func (w *LyricsWindow) drawLoading(bx, by, bw, bh float64, accent color.RGBA) {
	p := w.paint
	cy := by + bh*0.42
	r := w.px(4)
	gap := w.px(16)
	total := gap*2 + r*2
	x := bx + (bw-total)/2

	phase := float64(time.Now().UnixMilli()%1200) / 1200
	for i := 0; i < 3; i++ {
		t := math.Mod(phase-float64(i)*0.16+1, 1)
		lift := 0.3 + 0.7*math.Max(0, math.Cos(2*math.Pi*t))
		cx := x + float64(i)*gap + r
		p.begin(cx-r, cy-r, r*2, r*2)
		p.circle(cx, cy, r)
		p.flat(scaleAlpha(premul(accent), lift))
	}

	face := w.fonts.face(regular, w.px(12.5))
	label := "Looking for lyrics…"
	drawText(p.dst, face, colTextMuted, bx+(bw-measure(face, label))/2, cy+w.px(34), label)
}

func (w *LyricsWindow) drawCentred(bx, by, bw, bh float64, title, sub string) {
	p := w.paint
	titleFace := w.fonts.face(semibold, w.px(15))
	subFace := w.fonts.face(regular, w.px(11.5))

	y := by + bh*0.42
	drawText(p.dst, titleFace, rgba(255, 255, 255, 0.80),
		bx+(bw-measure(titleFace, truncate(titleFace, title, bw)))/2, y,
		truncate(titleFace, title, bw))
	if sub != "" {
		s := truncate(subFace, sub, bw)
		drawText(p.dst, subFace, colTextMuted, bx+(bw-measure(subFace, s))/2, y+w.px(22), s)
	}
}

// footerMetrics is the geometry of the progress row, computed once and used
// by both the painter and the hit test - the rail you can grab is then exactly
// the rail you can see, including as the clock digits change width.
func (w *LyricsWindow) footerMetrics() (railX, railW, midY, colW float64, known bool) {
	face := w.fonts.face(regular, w.px(10.5))
	m := w.px(lyrMargin)
	cx, cw := m, float64(w.win.w)-2*m
	cardBottom := float64(w.win.h) - m

	total := w.track.Duration
	known = total > 0
	colW = measure(face, "0:00")
	if known {
		colW = math.Max(colW, math.Max(
			measure(face, formatClock(w.position(time.Now()))),
			measure(face, formatClock(total))))
	}

	pad, gap := w.px(22), w.px(11)
	midY = cardBottom - w.px(20)
	railX = cx + pad + colW + gap
	railW = cx + cw - pad - colW - gap - railX
	return railX, railW, midY, colW, known
}

// drawFooter is the same progress row the overlay card uses, so the two
// surfaces agree on what a playhead looks like. Unlike the card's, this one is
// a control: hovering thickens it and dragging scrubs the track.
func (w *LyricsWindow) drawFooter(cx, cardBottom, cw float64, accent color.RGBA) {
	p := w.paint
	face := w.fonts.face(regular, w.px(10.5))
	total := w.track.Duration
	elapsed := w.position(time.Now())

	railX, railW, midY, _, known := w.footerMetrics()
	pad := w.px(22)

	if known {
		el, tot := formatClock(elapsed), formatClock(total)
		// The elapsed clock reads the scrub target while dragging, which is
		// what makes the rail usable: you can see where you are about to land.
		col := colTextMuted
		if w.scrubbing {
			col = colText
		}
		baseline := midY + w.px(3.8)
		drawText(p.dst, face, col, cx+pad, baseline, el)
		drawText(p.dst, face, colTextMuted, cx+cw-pad-measure(face, tot), baseline, tot)
	}

	// The rail grows under the pointer. It is the only affordance saying this
	// row can be dragged, so it has to be felt before it is understood.
	h := w.px(4)
	if w.railHot || w.scrubbing {
		h = w.px(6)
	}
	rad := h / 2
	y := midY - h/2
	if railW <= h {
		return
	}

	p.begin(railX, y, railW, h)
	p.roundRect(railX, y, railW, h, rad)
	p.flat(colProgress)
	if !known {
		return
	}

	frac := clamp01(float64(elapsed) / float64(total))
	fw := math.Max(railW*frac, h)
	p.begin(railX, y, fw, h)
	p.roundRect(railX, y, fw, h, rad)
	p.linear(railX, 0, railX+railW, 0,
		scaleAlpha(premul(accent), 0.7), premul(lighten(accent, 0.25)))

	capR := w.px(3.6)
	if w.railHot || w.scrubbing {
		capR = w.px(5.4)
	}
	capX := math.Min(math.Max(railX+fw, railX+capR), railX+railW-capR)
	glowR := capR * 2.8
	p.begin(capX-glowR, midY-glowR, glowR*2, glowR*2)
	p.circle(capX, midY, glowR)
	p.radial(capX, midY, glowR, scaleAlpha(premul(accent), 0.55))
	p.begin(capX-capR, midY-capR, capR*2, capR*2)
	p.circle(capX, midY, capR)
	p.flat(colText)
}

// drawGrip is the resize affordance: three diagonal dashes in the corner.
func (w *LyricsWindow) drawGrip(right, bottom float64) {
	p := w.paint
	step := w.px(4.5)
	th := math.Max(w.px(1.4), 1)
	for i := 1; i <= 3; i++ {
		l := float64(i) * step * 1.5
		x0, y0 := right-w.px(7)-l, bottom-w.px(7)
		x1, y1 := right-w.px(7), bottom-w.px(7)-l
		p.begin(x0-th, y1-th, l+2*th, l+2*th)
		p.polygon(x0, y0, x0+th, y0+th, x1+th, y1+th, x1, y1)
		p.flat(scaleAlpha(colLyrGrip, 1-0.22*float64(i-1)))
	}
}

// ---------------------------------------------------------------------------
// Input

// hitZone maps a point in client coordinates onto what is there.
func (w *LyricsWindow) hitZone(x, y int) zone {
	fx, fy := float64(x), float64(y)
	W, H := float64(w.win.w), float64(w.win.h)
	m := w.px(lyrMargin)
	edge := w.px(lyrEdge)
	grip := w.px(lyrGripSize)

	switch {
	case fx > W-m-grip && fy > H-m-grip:
		return zoneGripCorner
	case fy > H-m-edge && fx > m && fx < W-m:
		return zoneGripBottom
	case fx > W-m-edge && fy > m && fy < H-m:
		return zoneGripCorner // the right edge resizes width the same way
	}

	closeX, closeY, closeR, openX, sliderX, sliderW, sliderY := w.headerMetrics()
	if math.Hypot(fx-closeX, fy-closeY) <= closeR+w.px(4) {
		return zoneClose
	}
	if w.track.URI != "" && math.Hypot(fx-openX, fy-closeY) <= closeR+w.px(4) {
		return zoneOpenSpotify
	}
	// The slider's grab band reaches past both ends and well above and below
	// the 3px track, which is far too thin to aim at.
	if fy > sliderY-w.px(10) && fy < sliderY+w.px(10) &&
		fx > sliderX-w.px(12) && fx < sliderX+sliderW+w.px(8) {
		return zoneOpacity
	}

	// The rail is a thin thing to hit, so the grab band is much taller than
	// the rail is drawn, and reaches a little past both ends.
	if railX, railW, midY, _, known := w.footerMetrics(); known && railW > 0 {
		grab := w.px(11)
		if fy > midY-grab && fy < midY+grab &&
			fx > railX-w.px(6) && fx < railX+railW+w.px(6) {
			return zoneRail
		}
	}

	if fy < m+w.px(lyrHeaderH) {
		return zoneHeader
	}
	return zoneBody
}

func (w *LyricsWindow) onMouseDown(x, y int) {
	if !w.win.visible {
		return
	}
	z := w.hitZone(x, y)

	if z == zoneBody && w.handleLineClick(x, y, time.Now()) {
		return // a double-click on a lyric line seeked; nothing to drag
	}

	sx, sy := cursorPos()

	switch z {
	case zoneClose:
		w.close()
		return
	case zoneOpenSpotify:
		w.openTrack()
		return
	case zoneGripCorner, zoneGripBottom:
		w.drag = dragResize
	case zoneRail:
		// Pressing anywhere on the rail jumps there immediately rather than
		// waiting for a drag; a click is just a drag of zero length.
		w.drag = dragSeek
		w.scrubbing = true
		w.scrubTo(x)
		w.dirty = true
	case zoneOpacity:
		w.drag = dragOpacity
		w.sliding = true
		w.slideTo(x)
	case zoneHeader:
		w.drag = dragMove
	default:
		w.drag = dragScroll
	}
	w.pointer = z
	w.dragX, w.dragY = sx, sy
	w.dragW, w.dragH = w.win.w, w.win.h
	captureMouse(w.win.hwnd)
}

// openTrack fires the "Open in Spotify" callback. It is a no-op, not a
// crash, when there is nothing playing or the caller never wired the
// callback up (the standalone lyrics preview does not touch Spotify at all).
func (w *LyricsWindow) openTrack() {
	if w.track.URI == "" || w.opts.OnOpenSpotify == nil {
		return
	}
	uri := w.track.URI
	go w.opts.OnOpenSpotify(uri)
}

// lineAt is the paragraph under a client-coordinate y, or -1 for none. Only
// synced lyrics have a paragraph worth seeking to, so an unsynced or absent
// doc always misses.
func (w *LyricsWindow) lineAt(y int) int {
	if w.doc == nil || !w.doc.Synced || len(w.para) == 0 {
		return -1
	}
	_, by, _, bh := w.bodyRect()
	fy := float64(y)
	if fy < by || fy > by+bh {
		return -1
	}
	for i := range w.para {
		top := by - w.scroll + w.para[i].top
		if fy >= top && fy < top+w.para[i].h {
			return i
		}
	}
	return -1
}

// handleLineClick tracks clicks on lyric lines and seeks when two land on
// the same line inside lyrDoubleClickWindow. now is a parameter rather than
// time.Now() so the pairing logic can be driven by a test clock.
//
// A track of unknown length has no rail to seek with either, so the same
// gate applies here: double-click seeking only exists where there is
// something to seek within.
func (w *LyricsWindow) handleLineClick(x, y int, now time.Time) bool {
	if w.track.Duration <= 0 {
		return false
	}
	idx := w.lineAt(y)

	double := idx >= 0 && idx == w.lastClickIdx && now.Sub(w.lastClickAt) <= lyrDoubleClickWindow
	if !double {
		w.lastClickAt, w.lastClickIdx = now, idx
		return false
	}
	// Consumed: a third click right behind a completed pair starts fresh
	// rather than re-triggering, the same way a real double-click does not
	// chain into a seek on every subsequent click.
	w.lastClickAt, w.lastClickIdx = time.Time{}, -1

	if w.doc == nil || idx >= len(w.doc.Lines) {
		return false
	}
	w.seekTo(w.doc.Lines[idx].At)
	return true
}

func (w *LyricsWindow) onMouseMove(x, y int) {
	if !w.win.visible {
		return
	}
	z := w.hitZone(x, y)
	if hot := z == zoneClose; hot != w.closeHot {
		w.closeHot = hot
		w.dirty = true
	}
	if hot := z == zoneRail; hot != w.railHot {
		w.railHot = hot
		w.dirty = true
	}
	if hot := z == zoneOpacity; hot != w.sliderHot {
		w.sliderHot = hot
		w.dirty = true
	}
	if hot := z == zoneOpenSpotify; hot != w.openHot {
		w.openHot = hot
		w.dirty = true
	}
	if w.drag == dragNone {
		w.hover = z
		return
	}

	sx, sy := cursorPos()
	dx, dy := sx-w.dragX, sy-w.dragY

	switch w.drag {
	case dragMove:
		// Position is applied by the next present rather than by SetWindowPos,
		// so the move and the repaint land in the same compositor update and
		// the panel never tears against its own content.
		w.win.x += dx
		w.win.y += dy
		w.dragX, w.dragY = sx, sy
		w.clampToScreen()
		w.dirty = true

	case dragResize:
		nw := clampi(w.dragW+dx, int(w.px(lyrMinW)), int(w.px(lyrMaxW)))
		nh := clampi(w.dragH+dy, int(w.px(lyrMinH)), int(w.px(lyrMaxH)))
		if w.pointer == zoneGripBottom {
			nw = w.win.w
		}
		if nw != w.win.w || nh != w.win.h {
			w.win.w, w.win.h = nw, nh
			w.wrapKey = "" // width changed: the text has to be re-wrapped
			w.dirty = true
		}

	case dragSeek:
		w.scrubTo(x)
		w.dirty = true

	case dragOpacity:
		w.slideTo(x)

	case dragScroll:
		if dy != 0 {
			w.scrollTo = w.clampScroll(w.scrollTo - float64(dy))
			w.manualTil = time.Now().Add(lyrManualHold)
			w.dragX, w.dragY = sx, sy
			w.dirty = true
		}
	}
}

func (w *LyricsWindow) onMouseUp() {
	if w.drag == dragNone {
		return
	}
	mode := w.drag
	w.drag = dragNone
	releaseMouse()

	switch mode {
	case dragMove, dragResize:
		if w.opts.OnGeometry != nil {
			w.opts.OnGeometry(w.win.x, w.win.y, w.win.w, w.win.h)
		}
	case dragSeek:
		w.commitSeek()
	case dragOpacity:
		w.sliding = false
		// Leave the percentage up for a moment: it is the confirmation that
		// the value you let go of is the value that stuck.
		w.sliderShow = time.Now().Add(1500 * time.Millisecond)
		w.dirty = true
		if w.opts.OnOpacity != nil {
			v := w.opts.Opacity
			go w.opts.OnOpacity(v)
		}
	}
}

// slideTo sets the panel's opacity from a pointer position on the slider.
// The change is live: the next frame is composed at the new value, which is
// the only honest way to pick a transparency.
func (w *LyricsWindow) slideTo(x int) {
	_, _, _, _, sliderX, sliderW, _ := w.headerMetrics()
	w.opts.Opacity = opacityAt(float64(x), sliderX, sliderW)
	w.dirty = true
}

// scrubTo moves the handle to a pointer position, in client coordinates.
func (w *LyricsWindow) scrubTo(x int) {
	railX, railW, _, _, known := w.footerMetrics()
	if !known {
		return
	}
	w.scrubPos = seekTarget(float64(x), railX, railW, w.track.Duration)
}

// commitSeek hands the dropped position to the daemon and adopts it locally,
// so the rail stays where it was let go instead of springing back while the
// request is in flight.
func (w *LyricsWindow) commitSeek() {
	if !w.scrubbing {
		return
	}
	pos := w.scrubPos
	w.scrubbing = false
	w.seekTo(pos)
}

// seekTo adopts pos as the playhead locally and reports it upstream. Both
// letting go of the rail's handle and double-clicking a lyric line land
// here, so a seek behaves identically no matter which one triggered it.
func (w *LyricsWindow) seekTo(pos time.Duration) {
	w.track.Position = pos
	w.track.PositionAt = time.Now()
	w.seekHold = time.Now().Add(seekHoldFor)
	w.manualTil = time.Time{} // let the highlight snap to the new spot
	w.dirty = true

	if w.opts.OnSeek != nil {
		go w.opts.OnSeek(pos)
	}
}

func (w *LyricsWindow) onWheel(notches float64) {
	if !w.win.visible {
		return
	}
	w.scrollTo = w.clampScroll(w.scrollTo - notches*w.px(lyrWheelStep))
	w.manualTil = time.Now().Add(lyrManualHold)
	w.dirty = true
}

// applyCursor gives each zone its own pointer. Returning true tells Windows
// the message is handled and its default arrow should not be restored.
func (w *LyricsWindow) applyCursor() bool {
	if !w.win.visible {
		return false
	}
	switch w.hover {
	case zoneGripCorner:
		setCursorShape(cursorSizeNWSE)
	case zoneGripBottom:
		setCursorShape(cursorSizeNS)
	case zoneClose, zoneRail, zoneOpacity, zoneOpenSpotify:
		setCursorShape(cursorHand)
	default:
		setCursorShape(cursorArrow)
	}
	return true
}

// clampToScreen keeps at least a corner of the panel reachable, so a panel
// dragged off the edge can always be dragged back.
func (w *LyricsWindow) clampToScreen() {
	vw, vh := virtualScreen()
	keep := int(w.px(60))
	if w.win.x > vw-keep {
		w.win.x = vw - keep
	}
	if w.win.y > vh-keep {
		w.win.y = vh - keep
	}
	if w.win.x+w.win.w < keep {
		w.win.x = keep - w.win.w
	}
	if w.win.y < 0 {
		w.win.y = 0
	}
}
