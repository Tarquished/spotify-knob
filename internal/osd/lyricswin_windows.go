package osd

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"
)

// Win32 plumbing for the lyrics panel.
//
// It is a layered window like the overlay card, but with the opposite input
// posture: the card is WS_EX_TRANSPARENT and never touchable, while this one
// has to be dragged, resized and clicked. WS_EX_NOACTIVATE is kept - clicking
// the panel must not pull focus away from a game or from Spotify - which is
// fine, because a no-activate window still receives mouse messages.
//
// Hit-testing comes free with per-pixel alpha: Windows routes clicks on fully
// transparent pixels of a layered window straight through to whatever is
// underneath, so the rounded corners are genuinely rounded to the mouse.

var (
	procSetCapture     = user32.NewProc("SetCapture")
	procReleaseCapture = user32.NewProc("ReleaseCapture")
	procLoadCursor     = user32.NewProc("LoadCursorW")
	procSetCursor      = user32.NewProc("SetCursor")
	procGetSystemMetrs = user32.NewProc("GetSystemMetrics")
)

const (
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmMouseWheel  = 0x020A
	wmSetCursor   = 0x0020

	idcArrow    = 32512
	idcSizeNWSE = 32642
	idcSizeNS   = 32645
	idcHand     = 32649

	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// lyricsWin owns the panel's window and its backing bitmap.
//
// The bitmap is allocated once at the largest size the panel may reach and
// never reallocated. Resizing then costs nothing: UpdateLayeredWindow is told
// to read a smaller region out of the same DIB, which is what makes dragging
// the corner smooth instead of a stutter of DIB rebuilds.
type lyricsWin struct {
	hwnd   uintptr
	memDC  uintptr
	bitmap uintptr
	oldBmp uintptr
	bits   unsafe.Pointer

	maxW, maxH int
	w, h       int
	x, y       int
	visible    bool
}

var (
	lyricsClassName *uint16
	lyricsClassErr  error
	lyricsClassOnce bool

	// lyricsOwner receives the window messages. Only one panel exists, and it
	// is created on the thread that pumps it, so this needs no locking.
	lyricsOwner *LyricsWindow

	cursorArrow    uintptr
	cursorSizeNWSE uintptr
	cursorSizeNS   uintptr
	cursorHand     uintptr
)

func registerLyricsClass() error {
	if lyricsClassOnce {
		return lyricsClassErr
	}
	lyricsClassOnce = true

	name, err := syscall.UTF16PtrFromString("SpotifyKnobLyrics")
	if err != nil {
		lyricsClassErr = err
		return err
	}
	lyricsClassName = name

	cursorArrow, _, _ = procLoadCursor.Call(0, idcArrow)
	cursorSizeNWSE, _, _ = procLoadCursor.Call(0, idcSizeNWSE)
	cursorSizeNS, _, _ = procLoadCursor.Call(0, idcSizeNS)
	cursorHand, _, _ = procLoadCursor.Call(0, idcHand)

	inst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(lyricsWndProc),
		Instance:  inst,
		ClassName: name,
	}
	if r, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		lyricsClassErr = fmt.Errorf("RegisterClassExW(lyrics): %w", err)
	}
	return lyricsClassErr
}

// lyricsWndProc turns window messages into calls on the panel. It runs on the
// panel's own thread, dispatched from its message pump, so it can touch the
// panel's state directly.
func lyricsWndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	o := lyricsOwner
	if o == nil {
		r, _, _ := procDefWindowProc.Call(hwnd, message, wParam, lParam)
		return r
	}

	switch message {
	case wmLButtonDown:
		o.onMouseDown(loWordSigned(lParam), hiWordSigned(lParam))
		return 0
	case wmMouseMove:
		o.onMouseMove(loWordSigned(lParam), hiWordSigned(lParam))
		return 0
	case wmLButtonUp:
		o.onMouseUp()
		return 0
	case wmMouseWheel:
		// The delta is the signed high word of wParam, in multiples of 120.
		o.onWheel(float64(int16(uint32(wParam)>>16)) / 120)
		return 0
	case wmSetCursor:
		if o.applyCursor() {
			return 1
		}
	}
	r, _, _ := procDefWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

func loWordSigned(v uintptr) int { return int(int16(uint32(v) & 0xFFFF)) }
func hiWordSigned(v uintptr) int { return int(int16(uint32(v) >> 16)) }

func setCursorShape(c uintptr) {
	if c != 0 {
		procSetCursor.Call(c)
	}
}

func newLyricsWin(maxW, maxH, w, h, x, y int) (*lyricsWin, error) {
	enableDPIAwareness()
	if err := registerLyricsClass(); err != nil {
		return nil, err
	}
	inst, _, _ := procGetModuleHandleW.Call(0)
	title, _ := syscall.UTF16PtrFromString("spotify-knob lyrics")

	hwnd, _, err := procCreateWindowEx.Call(
		uintptr(wsExLayered|wsExToolWindow|wsExTopmost|wsExNoActivate),
		uintptr(unsafe.Pointer(lyricsClassName)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup),
		uintptr(int32(x)), uintptr(int32(y)), uintptr(w), uintptr(h),
		0, 0, inst, 0,
	)
	if hwnd == 0 {
		return nil, fmt.Errorf("CreateWindowExW(lyrics): %w", err)
	}

	lw := &lyricsWin{hwnd: hwnd, maxW: maxW, maxH: maxH, w: w, h: h, x: x, y: y}

	screen, _, _ := procGetDC.Call(0)
	lw.memDC, _, _ = procCreateCompatibleDC.Call(screen)
	if screen != 0 {
		procReleaseDC.Call(0, screen)
	}

	bi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(maxW),
		Height:      -int32(maxH), // top-down, matching image.RGBA
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	var bits unsafe.Pointer
	bmp, _, err := procCreateDIBSection.Call(
		lw.memDC,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if bmp == 0 {
		lw.destroy()
		return nil, fmt.Errorf("CreateDIBSection(lyrics): %w", err)
	}
	lw.bitmap = bmp
	lw.bits = bits
	lw.oldBmp, _, _ = procSelectObject.Call(lw.memDC, bmp)
	return lw, nil
}

// present copies the rendered panel into the DIB and hands it to the
// compositor at the panel's current size and position.
func (lw *lyricsWin) present(img *image.RGBA, opacity float64) error {
	if lw.bits == nil {
		return fmt.Errorf("no backing bitmap")
	}
	if opacity < 0 {
		opacity = 0
	} else if opacity > 1 {
		opacity = 1
	}
	alpha := uint32(opacity*256 + 0.5)

	dst := unsafe.Slice((*byte)(lw.bits), lw.maxW*lw.maxH*4)
	src := img.Pix
	srcStride := img.Stride
	dstStride := lw.maxW * 4
	rowBytes := lw.w * 4

	for row := 0; row < lw.h; row++ {
		out := dst[row*dstStride : row*dstStride+rowBytes]
		in := src[row*srcStride : row*srcStride+rowBytes]
		for i := 0; i < rowBytes; i += 4 {
			// Premultiplied throughout, so the window-wide opacity is a plain
			// scale of all four channels. RGBA in, BGRA out.
			out[i+0] = uint8(uint32(in[i+2]) * alpha >> 8)
			out[i+1] = uint8(uint32(in[i+1]) * alpha >> 8)
			out[i+2] = uint8(uint32(in[i+0]) * alpha >> 8)
			out[i+3] = uint8(uint32(in[i+3]) * alpha >> 8)
		}
	}

	ptDst := point{X: int32(lw.x), Y: int32(lw.y)}
	ptSrc := point{X: 0, Y: 0}
	sz := size{CX: int32(lw.w), CY: int32(lw.h)}
	blend := blendFunction{
		BlendOp:             acSrcOver,
		SourceConstantAlpha: 255,
		AlphaFormat:         acSrcAlpha,
	}

	// hdcDst NULL, for the same reason as the overlay card: a screen DC held
	// across frames goes stale and composites without alpha.
	r, _, err := procUpdateLayeredWin.Call(
		lw.hwnd,
		0,
		uintptr(unsafe.Pointer(&ptDst)),
		uintptr(unsafe.Pointer(&sz)),
		lw.memDC,
		uintptr(unsafe.Pointer(&ptSrc)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ulwAlpha,
	)
	if r == 0 {
		return fmt.Errorf("UpdateLayeredWindow(lyrics): %w", err)
	}
	if !lw.visible {
		procShowWindow.Call(lw.hwnd, swShowNoActivate)
		lw.visible = true
	}
	return nil
}

func (lw *lyricsWin) hide() {
	if !lw.visible {
		return
	}
	procShowWindow.Call(lw.hwnd, swHide)
	lw.visible = false
}

func (lw *lyricsWin) reassertTopmost() {
	if lw.visible {
		procSetWindowPos.Call(lw.hwnd, hwndTopmost, 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoActivate)
	}
}

func (lw *lyricsWin) destroy() {
	if lw.oldBmp != 0 {
		procSelectObject.Call(lw.memDC, lw.oldBmp)
		lw.oldBmp = 0
	}
	if lw.bitmap != 0 {
		procDeleteObject.Call(lw.bitmap)
		lw.bitmap = 0
	}
	if lw.memDC != 0 {
		procDeleteDC.Call(lw.memDC)
		lw.memDC = 0
	}
	if lw.hwnd != 0 {
		procDestroyWindow.Call(lw.hwnd)
		lw.hwnd = 0
	}
	lw.bits = nil
}

func (lw *lyricsWin) pump() {
	var m msg
	for {
		r, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func captureMouse(hwnd uintptr) { procSetCapture.Call(hwnd) }
func releaseMouse()             { procReleaseCapture.Call() }

// virtualScreen is the bounding box of every monitor, used to keep a dragged
// panel from being parked entirely off-screen.
func virtualScreen() (w, h int) {
	cx, _, _ := procGetSystemMetrs.Call(smCXVirtualScreen)
	cy, _, _ := procGetSystemMetrs.Call(smCYVirtualScreen)
	if cx == 0 || cy == 0 {
		return 1920, 1080
	}
	return int(int32(cx)), int(int32(cy))
}

// workArea is the usable area of the monitor nearest a point, excluding the
// taskbar. The panel opens inside it rather than under the taskbar.
func workAreaAt(x, y int) (left, top, right, bottom int) {
	pt := point{X: int32(x), Y: int32(y)}
	mon, _, _ := procMonitorFromPoint.Call(
		uintptr(*(*uint64)(unsafe.Pointer(&pt))), monitorDefaultToNearest)
	if mon == 0 {
		w, h := virtualScreen()
		return 0, 0, w, h
	}
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		w, h := virtualScreen()
		return 0, 0, w, h
	}
	return int(mi.Work.Left), int(mi.Work.Top), int(mi.Work.Right), int(mi.Work.Bottom)
}
