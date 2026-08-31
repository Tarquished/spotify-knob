package osd

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/vector"
)

// Compositing for the card.
//
// The first version of this file handed gradients to the rasteriser as
// image.Image values. That is the idiomatic way to do it, and it was far too
// slow: profiling showed 70% of a frame inside the rasteriser's generic path,
// which calls At() per pixel and boxes a color.Color into an interface each
// time - one heap allocation per pixel, ~120k per frame.
//
// So every non-flat fill here rasterises into a reusable coverage mask and
// then runs a tight, allocation-free composite loop over the shape's bounding
// box. Flat fills still go through the rasteriser, which has a fast path for
// a uniform colour on an RGBA destination.

// painter owns the frame buffer and the scratch buffers reused across shapes.
type painter struct {
	dst  *image.RGBA
	rast *vector.Rasterizer
	mask *image.Alpha
	uni  *image.Uniform
	rect image.Rectangle // bounds of the shape being built
	w, h int
}

func newPainter(w, h int) *painter {
	return &painter{
		dst:  image.NewRGBA(image.Rect(0, 0, w, h)),
		rast: vector.NewRasterizer(w, h),
		mask: image.NewAlpha(image.Rect(0, 0, w, h)),
		uni:  image.NewUniform(color.Alpha{A: 255}),
		w:    w,
		h:    h,
	}
}

func (p *painter) clear() {
	pix := p.dst.Pix
	for i := range pix {
		pix[i] = 0
	}
}

// clearRect zeroes only the top-left w by h pixels. The lyrics panel keeps a
// canvas sized for the largest it may ever be, so clearing all of it every
// frame would be several megabytes of memset for a window a quarter that big.
func (p *painter) clearRect(w, h int) {
	if w > p.w {
		w = p.w
	}
	if h > p.h {
		h = p.h
	}
	stride := p.dst.Stride
	row := w * 4
	for y := 0; y < h; y++ {
		line := p.dst.Pix[y*stride : y*stride+row]
		for i := range line {
			line[i] = 0
		}
	}
}

// begin starts a shape confined to the given rectangle. Sizing the rasteriser
// to the shape instead of the whole canvas is most of the speed: a 6px tall
// volume bar no longer costs a full-canvas clear.
func (p *painter) begin(x, y, w, h float64) {
	r := image.Rect(
		int(math.Floor(x))-1, int(math.Floor(y))-1,
		int(math.Ceil(x+w))+1, int(math.Ceil(y+h))+1,
	).Intersect(p.dst.Bounds())
	p.rect = r
	if r.Empty() {
		return
	}
	p.rast.Reset(r.Dx(), r.Dy())
}

// Path building. Coordinates are absolute; the offset into the shape's own
// raster space is applied here.

func (p *painter) roundRect(x, y, w, h, rad float64) {
	ox, oy := p.origin()
	roundRect(p.rast, float32(x)-ox, float32(y)-oy, float32(w), float32(h), float32(rad))
}

func (p *painter) roundRectRev(x, y, w, h, rad float64) {
	ox, oy := p.origin()
	roundRectRev(p.rast, float32(x)-ox, float32(y)-oy, float32(w), float32(h), float32(rad))
}

func (p *painter) circle(cx, cy, rad float64) {
	ox, oy := p.origin()
	circle(p.rast, float32(cx)-ox, float32(cy)-oy, float32(rad))
}

// circleRev winds a circle the other way, so adding it to a path already
// holding a larger circle punches a hole and leaves a ring.
func (p *painter) circleRev(cx, cy, rad float64) {
	ox, oy := p.origin()
	circleRev(p.rast, float32(cx)-ox, float32(cy)-oy, float32(rad))
}

func (p *painter) polygon(pts ...float64) {
	ox, oy := p.origin()
	fs := make([]float32, len(pts))
	for i, v := range pts {
		if i%2 == 0 {
			fs[i] = float32(v) - ox
		} else {
			fs[i] = float32(v) - oy
		}
	}
	polygon(p.rast, fs...)
}

func (p *painter) origin() (float32, float32) {
	return float32(p.rect.Min.X), float32(p.rect.Min.Y)
}

// flat fills the current path with one colour, through the rasteriser's fast
// uniform-on-RGBA path.
func (p *painter) flat(col color.RGBA) {
	if p.rect.Empty() || col.A == 0 {
		return
	}
	p.uni.C = col
	p.rast.DrawOp = draw.Over
	p.rast.Draw(p.dst, p.rect, p.uni, image.Point{})
}

// coverage rasterises the current path into the shared mask. DrawOp is Src,
// so the mask needs no clearing: every pixel in the rectangle is written.
func (p *painter) coverage() {
	p.uni.C = color.Alpha{A: 255}
	p.rast.DrawOp = draw.Src
	p.rast.Draw(p.mask, p.rect, p.uni, image.Point{})
	p.rast.DrawOp = draw.Over
}

// linear fills the current path with a gradient between two premultiplied
// colours along the vector (x0,y0)->(x1,y1).
func (p *painter) linear(x0, y0, x1, y1 float64, from, to color.RGBA) {
	if p.rect.Empty() {
		return
	}
	p.coverage()

	dx, dy := x1-x0, y1-y0
	den := dx*dx + dy*dy
	if den == 0 {
		den = 1
	}
	// t advances linearly across a row, so only the row term is recomputed.
	stepX := dx / den

	fr, fg, fb, fa := float64(from.R), float64(from.G), float64(from.B), float64(from.A)
	dr, dg, db, da := float64(to.R)-fr, float64(to.G)-fg, float64(to.B)-fb, float64(to.A)-fa

	r := p.rect
	for y := r.Min.Y; y < r.Max.Y; y++ {
		t := ((float64(r.Min.X)-x0)*dx + (float64(y)-y0)*dy) / den
		mi := p.mask.PixOffset(r.Min.X, y)
		di := p.dst.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			if cov := uint32(p.mask.Pix[mi]); cov != 0 {
				u := t
				if u < 0 {
					u = 0
				} else if u > 1 {
					u = 1
				}
				blend(p.dst.Pix, di,
					uint32(fr+dr*u), uint32(fg+dg*u), uint32(fb+db*u), uint32(fa+da*u),
					cov)
			}
			t += stepX
			mi++
			di += 4
		}
	}
}

// radial fills the current path with a colour fading out from a centre point,
// used for the accent halo behind the artwork.
func (p *painter) radial(cx, cy, rad float64, col color.RGBA) {
	if p.rect.Empty() || rad <= 0 {
		return
	}
	p.coverage()

	cr, cg, cb, ca := float64(col.R), float64(col.G), float64(col.B), float64(col.A)
	inv := 1 / rad

	r := p.rect
	for y := r.Min.Y; y < r.Max.Y; y++ {
		dy := float64(y) - cy
		dy2 := dy * dy
		mi := p.mask.PixOffset(r.Min.X, y)
		di := p.dst.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			if cov := uint32(p.mask.Pix[mi]); cov != 0 {
				dx := float64(x) - cx
				d := math.Sqrt(dx*dx+dy2) * inv
				if d < 1 {
					f := (1 - d) * (1 - d) // squared falloff: gentler shoulder
					blend(p.dst.Pix, di,
						uint32(cr*f), uint32(cg*f), uint32(cb*f), uint32(ca*f),
						cov)
				}
			}
			mi++
			di += 4
		}
	}
}

// picture fills the current path with an image placed at (ox, oy), scaled by
// a uniform fade. This is how the album cover gets its rounded corners.
func (p *painter) picture(img *image.RGBA, ox, oy float64, fade float64) {
	if p.rect.Empty() || img == nil || fade <= 0 {
		return
	}
	p.coverage()
	if fade > 1 {
		fade = 1
	}

	ib := img.Bounds()
	offX, offY := int(ox), int(oy)

	r := p.rect
	for y := r.Min.Y; y < r.Max.Y; y++ {
		sy := y - offY + ib.Min.Y
		if sy < ib.Min.Y || sy >= ib.Max.Y {
			continue
		}
		mi := p.mask.PixOffset(r.Min.X, y)
		di := p.dst.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			sx := x - offX + ib.Min.X
			if cov := uint32(p.mask.Pix[mi]); cov != 0 && sx >= ib.Min.X && sx < ib.Max.X {
				si := img.PixOffset(sx, sy)
				blend(p.dst.Pix, di,
					uint32(float64(img.Pix[si+0])*fade),
					uint32(float64(img.Pix[si+1])*fade),
					uint32(float64(img.Pix[si+2])*fade),
					uint32(float64(img.Pix[si+3])*fade),
					cov)
			}
			mi++
			di += 4
		}
	}
}

// blitMask composites a flat colour through a standalone mask, used for the
// blurred shadow and the bar's bloom.
func (p *painter) blitMask(m *image.Alpha, col color.RGBA) {
	if m == nil || col.A == 0 {
		return
	}
	r := m.Bounds().Intersect(p.dst.Bounds())
	sr, sg, sb, sa := uint32(col.R), uint32(col.G), uint32(col.B), uint32(col.A)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		mi := m.PixOffset(r.Min.X, y)
		di := p.dst.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			if cov := uint32(m.Pix[mi]); cov != 0 {
				blend(p.dst.Pix, di, sr, sg, sb, sa, cov)
			}
			mi++
			di += 4
		}
	}
}

// blend does premultiplied source-over of one pixel, weighted by coverage.
func blend(pix []uint8, i int, sr, sg, sb, sa, cov uint32) {
	if cov != 255 {
		sr = sr * cov / 255
		sg = sg * cov / 255
		sb = sb * cov / 255
		sa = sa * cov / 255
	}
	if sa == 0 && sr == 0 && sg == 0 && sb == 0 {
		return
	}
	ia := 255 - sa
	pix[i+0] = uint8(sr + uint32(pix[i+0])*ia/255)
	pix[i+1] = uint8(sg + uint32(pix[i+1])*ia/255)
	pix[i+2] = uint8(sb + uint32(pix[i+2])*ia/255)
	pix[i+3] = uint8(sa + uint32(pix[i+3])*ia/255)
}

// Path primitives, in the rasteriser's local coordinate space.

// roundRect appends a rounded rectangle, clockwise.
func roundRect(r *vector.Rasterizer, x, y, w, h, rad float32) {
	if rad > w/2 {
		rad = w / 2
	}
	if rad > h/2 {
		rad = h / 2
	}
	if rad < 0 {
		rad = 0
	}
	const k = 0.5522847498 // circular arc as a cubic Bézier
	c := rad * k

	r.MoveTo(x+rad, y)
	r.LineTo(x+w-rad, y)
	r.CubeTo(x+w-rad+c, y, x+w, y+rad-c, x+w, y+rad)
	r.LineTo(x+w, y+h-rad)
	r.CubeTo(x+w, y+h-rad+c, x+w-rad+c, y+h, x+w-rad, y+h)
	r.LineTo(x+rad, y+h)
	r.CubeTo(x+rad-c, y+h, x, y+h-rad+c, x, y+h-rad)
	r.LineTo(x, y+rad)
	r.CubeTo(x, y+rad-c, x+rad-c, y, x+rad, y)
	r.ClosePath()
}

// roundRectRev traces the same shape counter-clockwise, so adding it to a
// clockwise path punches a hole under the non-zero winding rule.
func roundRectRev(r *vector.Rasterizer, x, y, w, h, rad float32) {
	if rad > w/2 {
		rad = w / 2
	}
	if rad > h/2 {
		rad = h / 2
	}
	if rad < 0 {
		rad = 0
	}
	const k = 0.5522847498
	c := rad * k

	r.MoveTo(x+rad, y)
	r.CubeTo(x+rad-c, y, x, y+rad-c, x, y+rad)
	r.LineTo(x, y+h-rad)
	r.CubeTo(x, y+h-rad+c, x+rad-c, y+h, x+rad, y+h)
	r.LineTo(x+w-rad, y+h)
	r.CubeTo(x+w-rad+c, y+h, x+w, y+h-rad+c, x+w, y+h-rad)
	r.LineTo(x+w, y+rad)
	r.CubeTo(x+w, y+rad-c, x+w-rad+c, y, x+w-rad, y)
	r.ClosePath()
}

func circle(r *vector.Rasterizer, cx, cy, rad float32) {
	roundRect(r, cx-rad, cy-rad, rad*2, rad*2, rad)
}

func circleRev(r *vector.Rasterizer, cx, cy, rad float32) {
	roundRectRev(r, cx-rad, cy-rad, rad*2, rad*2, rad)
}

func polygon(r *vector.Rasterizer, pts ...float32) {
	if len(pts) < 6 {
		return
	}
	r.MoveTo(pts[0], pts[1])
	for i := 2; i+1 < len(pts); i += 2 {
		r.LineTo(pts[i], pts[i+1])
	}
	r.ClosePath()
}

// blurredMask rasterises a path and blurs it, returning a mask positioned in
// absolute canvas coordinates. Used for shadows and glows, both of which are
// cached because blurring is not something to do 144 times a second.
func blurredMask(bounds image.Rectangle, radius int, build func(*vector.Rasterizer, float32, float32)) *image.Alpha {
	if bounds.Empty() {
		return nil
	}
	w, h := bounds.Dx(), bounds.Dy()
	rast := vector.NewRasterizer(w, h)
	build(rast, float32(bounds.Min.X), float32(bounds.Min.Y))

	local := image.NewAlpha(image.Rect(0, 0, w, h))
	rast.DrawOp = draw.Src
	rast.Draw(local, local.Bounds(), image.NewUniform(color.Alpha{A: 255}), image.Point{})

	local = blurAlpha(local, radius)
	// Reposition into canvas coordinates without copying the pixels.
	return &image.Alpha{Pix: local.Pix, Stride: local.Stride, Rect: bounds}
}

// blurAlpha box-blurs a coverage mask. Three passes approximate a Gaussian
// closely enough for a shadow.
func blurAlpha(m *image.Alpha, radius int) *image.Alpha {
	if radius < 1 {
		return m
	}
	for i := 0; i < 3; i++ {
		m = boxBlurH(m, radius)
		m = boxBlurV(m, radius)
	}
	return m
}

func boxBlurH(src *image.Alpha, radius int) *image.Alpha {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewAlpha(b)
	win := radius*2 + 1
	for y := 0; y < h; y++ {
		row := src.Pix[y*src.Stride : y*src.Stride+w]
		out := dst.Pix[y*dst.Stride : y*dst.Stride+w]
		sum := 0
		for i := -radius; i <= radius; i++ {
			sum += int(row[clampi(i, 0, w-1)])
		}
		for x := 0; x < w; x++ {
			out[x] = uint8(sum / win)
			sum -= int(row[clampi(x-radius, 0, w-1)])
			sum += int(row[clampi(x+radius+1, 0, w-1)])
		}
	}
	return dst
}

func boxBlurV(src *image.Alpha, radius int) *image.Alpha {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewAlpha(b)
	win := radius*2 + 1
	for x := 0; x < w; x++ {
		sum := 0
		for i := -radius; i <= radius; i++ {
			sum += int(src.Pix[clampi(i, 0, h-1)*src.Stride+x])
		}
		for y := 0; y < h; y++ {
			dst.Pix[y*dst.Stride+x] = uint8(sum / win)
			sum -= int(src.Pix[clampi(y-radius, 0, h-1)*src.Stride+x])
			sum += int(src.Pix[clampi(y+radius+1, 0, h-1)*src.Stride+x])
		}
	}
	return dst
}

func clampi(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// rgba builds a premultiplied colour from straight components and an 0-1
// alpha, which is how the theme is written.
func rgba(r, g, b uint8, a float64) color.RGBA {
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	return color.RGBA{
		R: uint8(float64(r) * a),
		G: uint8(float64(g) * a),
		B: uint8(float64(b) * a),
		A: uint8(255 * a),
	}
}

// scaleAlpha multiplies a premultiplied colour, keeping it premultiplied.
func scaleAlpha(c color.RGBA, f float64) color.RGBA {
	if f <= 0 {
		return color.RGBA{}
	}
	if f > 1 {
		f = 1
	}
	return color.RGBA{
		R: uint8(float64(c.R) * f),
		G: uint8(float64(c.G) * f),
		B: uint8(float64(c.B) * f),
		A: uint8(float64(c.A) * f),
	}
}

// premul turns an opaque colour into the premultiplied form the canvas uses.
func premul(c color.RGBA) color.RGBA {
	if c.A == 255 {
		return c
	}
	a := float64(c.A) / 255
	return color.RGBA{
		R: uint8(float64(c.R) * a),
		G: uint8(float64(c.G) * a),
		B: uint8(float64(c.B) * a),
		A: c.A,
	}
}

// lerpColor blends two premultiplied colours by t in 0..1, clamped at both
// ends so a caller does not have to clamp its own t first.
func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	return color.RGBA{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t),
		A: uint8(float64(a.A) + (float64(b.A)-float64(a.A))*t),
	}
}

func lighten(c color.RGBA, f float64) color.RGBA {
	return color.RGBA{
		R: uint8(float64(c.R) + (255-float64(c.R))*f),
		G: uint8(float64(c.G) + (255-float64(c.G))*f),
		B: uint8(float64(c.B) + (255-float64(c.B))*f),
		A: c.A,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
