package osd

import (
	"fmt"
	"image"
	"math"
	"sync"
	"syscall"
	"unsafe"
)

// Win32 plumbing for a layered, click-through, always-on-top overlay.
//
// The window never becomes active and never receives input: WS_EX_TRANSPARENT
// passes clicks to whatever is underneath, WS_EX_NOACTIVATE keeps focus where
// it was (critical while gaming), and WS_EX_TOOLWINDOW keeps it out of
// Alt+Tab. Its pixels are handed to the compositor wholesale through
// UpdateLayeredWindow, so there is no WM_PAINT and no flicker.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassEx   = user32.NewProc("RegisterClassExW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procShowWindow        = user32.NewProc("ShowWindow")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procGetDC             = user32.NewProc("GetDC")
	procReleaseDC         = user32.NewProc("ReleaseDC")
	procUpdateLayeredWin  = user32.NewProc("UpdateLayeredWindow")
	procPeekMessage       = user32.NewProc("PeekMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessage   = user32.NewProc("DispatchMessageW")
	procGetForegroundWin  = user32.NewProc("GetForegroundWindow")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
	procMonitorFromWindow = user32.NewProc("MonitorFromWindow")
	procMonitorFromPoint  = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfo    = user32.NewProc("GetMonitorInfoW")
	procGetClassName      = user32.NewProc("GetClassNameW")
	procSetDpiAwareness   = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem   = user32.NewProc("GetDpiForSystem")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procGetAsyncKeyState  = user32.NewProc("GetAsyncKeyState")
	procGetCursorPos      = user32.NewProc("GetCursorPos")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")

	procSHQueryNotifyState = shell32.NewProc("SHQueryUserNotificationState")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wsPopup = 0x80000000

	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExToolWindow  = 0x00000080
	wsExTopmost     = 0x00000008
	wsExNoActivate  = 0x08000000

	swHide           = 0
	swShowNoActivate = 4

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010

	hwndTopmost = ^uintptr(0) // (HWND)-1

	pmRemove = 0x0001

	ulwAlpha     = 0x00000002
	acSrcOver    = 0x00
	acSrcAlpha   = 0x01
	biRGB        = 0
	dibRGBColors = 0

	logPixelsX = 88
	vRefresh   = 116

	vkLButton = 0x01
	vkRButton = 0x02
	vkMButton = 0x04

	monitorDefaultToNearest = 2
	monitorDefaultToPrimary = 1

	// SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)
	dpiAwarePerMonitorV2 = ^uintptr(3) // (HANDLE)-4
)

type rect struct{ Left, Top, Right, Bottom int32 }

type point struct{ X, Y int32 }

type size struct{ CX, CY int32 }

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

var dpiOnce sync.Once

// enableDPIAwareness opts the process into per-monitor DPI so the overlay is
// rendered at native resolution instead of being bitmap-stretched by Windows.
func enableDPIAwareness() {
	dpiOnce.Do(func() {
		if procSetDpiAwareness.Find() == nil {
			procSetDpiAwareness.Call(dpiAwarePerMonitorV2)
		}
	})
}

// displayScale is the system scale factor (1.0 at 96 DPI).
func displayScale() float64 {
	enableDPIAwareness()
	if procGetDpiForSystem.Find() == nil {
		if dpi, _, _ := procGetDpiForSystem.Call(); dpi > 0 {
			return float64(dpi) / 96
		}
	}
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 1
	}
	defer procReleaseDC.Call(0, hdc)
	dpi, _, _ := procGetDeviceCaps.Call(hdc, logPixelsX)
	if dpi == 0 {
		return 1
	}
	return float64(dpi) / 96
}

// mouseDown tracks the previous poll, so only the press edge counts. Only the
// overlay thread touches it.
var mouseDown bool

// foregroundWindow is whichever window currently has focus.
func foregroundWindow() uintptr {
	h, _, _ := procGetForegroundWin.Call()
	return h
}

// cursorPos is where the pointer is right now.
func cursorPos() (int, int) {
	var pt point
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
		return -1 << 30, -1 << 30
	}
	return int(pt.X), int(pt.Y)
}

// mouseClicked reports a mouse button going down since the last call.
func mouseClicked() bool {
	down := false
	for _, vk := range []uintptr{vkLButton, vkRButton, vkMButton} {
		if state, _, _ := procGetAsyncKeyState.Call(vk); state&0x8000 != 0 {
			down = true
			break
		}
	}
	edge := down && !mouseDown
	mouseDown = down
	return edge
}

// primeMouse records the current button state without reporting an edge, so a
// button already held when the card appears does not dismiss it instantly.
func primeMouse() { mouseClicked() }

// refreshRate reports the active monitor refresh rate in Hz, so the animation
// can be paced to the display instead of a hardcoded 60.
func refreshRate() int {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 60
	}
	defer procReleaseDC.Call(0, hdc)
	hz, _, _ := procGetDeviceCaps.Call(hdc, vRefresh)
	// 0 and 1 both mean "default hardware rate" in the GDI API.
	if hz <= 1 {
		return 60
	}
	return int(hz)
}

type window struct {
	hwnd    uintptr
	memDC   uintptr
	bitmap  uintptr
	oldBmp  uintptr
	bits    unsafe.Pointer
	w, h    int
	visible bool
	zeroRow []uint8 // stand-in for a source row that is off the top or bottom
}

var (
	classOnce sync.Once
	classErr  error
	className *uint16
)

func registerClass() error {
	classOnce.Do(func() {
		name, err := syscall.UTF16PtrFromString("SpotifyKnobOSD")
		if err != nil {
			classErr = err
			return
		}
		className = name
		inst, _, _ := procGetModuleHandleW.Call(0)
		wc := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:   syscall.NewCallback(wndProc),
			Instance:  inst,
			ClassName: name,
		}
		if r, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = fmt.Errorf("RegisterClassExW: %w", err)
		}
	})
	return classErr
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

func newWindow(w, h int) (*window, error) {
	enableDPIAwareness()
	if err := registerClass(); err != nil {
		return nil, err
	}
	inst, _, _ := procGetModuleHandleW.Call(0)
	title, _ := syscall.UTF16PtrFromString("spotify-knob")

	hwnd, _, err := procCreateWindowEx.Call(
		uintptr(wsExLayered|wsExTransparent|wsExToolWindow|wsExTopmost|wsExNoActivate),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup),
		0, 0, uintptr(w), uintptr(h),
		0, 0, inst, 0,
	)
	if hwnd == 0 {
		return nil, fmt.Errorf("CreateWindowExW: %w", err)
	}

	win := &window{hwnd: hwnd, w: w, h: h, zeroRow: make([]uint8, w*4)}

	// A screen DC is needed to create a compatible memory DC, and only for
	// that: it is released immediately rather than held for the window's life.
	screen, _, _ := procGetDC.Call(0)
	win.memDC, _, _ = procCreateCompatibleDC.Call(screen)
	if screen != 0 {
		procReleaseDC.Call(0, screen)
	}

	bi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      -int32(h), // negative: top-down, matching image.RGBA
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	var bits unsafe.Pointer
	bmp, _, err := procCreateDIBSection.Call(
		win.memDC,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if bmp == 0 {
		win.destroy()
		return nil, fmt.Errorf("CreateDIBSection: %w", err)
	}
	win.bitmap = bmp
	win.bits = bits
	win.oldBmp, _, _ = procSelectObject.Call(win.memDC, bmp)
	return win, nil
}

// present composes the final frame into the DIB and hands it to the
// compositor, moving the window to (x, y) in the same call.
//
// The slide and the fade are applied here rather than during composition.
// Both are a single pass over the buffer that has to happen anyway for the
// RGBA-to-BGRA swap, so an animation frame costs nothing beyond the copy that
// was already required. offsetY is interpolated between rows, so the card
// slides at sub-pixel resolution instead of stepping a pixel at a time.
func (w *window) present(img *image.RGBA, x, y int, offsetY, opacity float64) error {
	if w.bits == nil {
		return fmt.Errorf("no backing bitmap")
	}
	if opacity < 0 {
		opacity = 0
	} else if opacity > 1 {
		opacity = 1
	}

	dst := unsafe.Slice((*byte)(w.bits), w.w*w.h*4)
	src := img.Pix
	stride := img.Stride
	rowBytes := w.w * 4

	shift := math.Floor(offsetY)
	frac := offsetY - shift
	k := int(shift)

	// Fixed-point weights for the two source rows, folded together with the
	// opacity so the inner loop is one multiply-add per channel.
	near := uint32((1-frac)*opacity*256 + 0.5)
	far := uint32(frac*opacity*256 + 0.5)

	for row := 0; row < w.h; row++ {
		di := row * rowBytes
		out := dst[di : di+rowBytes]

		nearRow, farRow := w.zeroRow, w.zeroRow
		if r := row - k; r >= 0 && r < img.Bounds().Dy() {
			nearRow = src[r*stride : r*stride+rowBytes]
		}
		if r := row - k - 1; r >= 0 && r < img.Bounds().Dy() {
			farRow = src[r*stride : r*stride+rowBytes]
		}

		for i := 0; i < rowBytes; i += 4 {
			r := uint32(nearRow[i+0])*near + uint32(farRow[i+0])*far
			g := uint32(nearRow[i+1])*near + uint32(farRow[i+1])*far
			b := uint32(nearRow[i+2])*near + uint32(farRow[i+2])*far
			a := uint32(nearRow[i+3])*near + uint32(farRow[i+3])*far
			// image.RGBA is R,G,B,A premultiplied; a DIB wants B,G,R,A.
			out[i+0] = uint8(b >> 8)
			out[i+1] = uint8(g >> 8)
			out[i+2] = uint8(r >> 8)
			out[i+3] = uint8(a >> 8)
		}
	}

	ptDst := point{X: int32(x), Y: int32(y)}
	ptSrc := point{X: 0, Y: 0}
	sz := size{CX: int32(w.w), CY: int32(w.h)}
	blend := blendFunction{
		BlendOp:             acSrcOver,
		SourceConstantAlpha: 255,
		AlphaFormat:         acSrcAlpha,
	}

	// hdcDst is NULL on purpose. Holding a screen DC from GetDC(0) for the
	// life of the window and passing it every frame is asking for trouble:
	// it goes stale when desktop composition changes, and a stale
	// destination can composite the frame without its alpha - a solid black
	// rectangle where the transparent padding should be.
	r, _, err := procUpdateLayeredWin.Call(
		w.hwnd,
		0,
		uintptr(unsafe.Pointer(&ptDst)),
		uintptr(unsafe.Pointer(&sz)),
		w.memDC,
		uintptr(unsafe.Pointer(&ptSrc)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ulwAlpha,
	)
	if r == 0 {
		return fmt.Errorf("UpdateLayeredWindow: %w", err)
	}

	// Only now make it visible. A layered window that is shown before it has
	// any content paints as a solid black rectangle, which is exactly what a
	// failed or not-yet-issued update used to leave on screen.
	if !w.visible {
		procShowWindow.Call(w.hwnd, swShowNoActivate)
		w.visible = true
	}
	return nil
}

// reassertTopmost puts the overlay back on top. Switching windows can push it
// down the z-order even though it never takes focus.
func (w *window) reassertTopmost() {
	if w.visible {
		procSetWindowPos.Call(w.hwnd, hwndTopmost, 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoActivate)
	}
}

func (w *window) hide() {
	if !w.visible {
		return
	}
	procShowWindow.Call(w.hwnd, swHide)
	w.visible = false
}

// pump services the window's message queue so Windows never sees it as hung.
func (w *window) pump() {
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

func (w *window) destroy() {
	if w.memDC != 0 {
		if w.oldBmp != 0 {
			procSelectObject.Call(w.memDC, w.oldBmp)
		}
		procDeleteDC.Call(w.memDC)
	}
	if w.bitmap != 0 {
		procDeleteObject.Call(w.bitmap)
	}
	if w.hwnd != 0 {
		procDestroyWindow.Call(w.hwnd)
	}
	*w = window{}
}

// workArea returns the usable desktop rectangle (taskbar excluded) of the
// monitor holding the foreground window, falling back to the primary one.
func workArea() rect {
	fg, _, _ := procGetForegroundWin.Call()
	var mon uintptr
	if fg != 0 {
		mon, _, _ = procMonitorFromWindow.Call(fg, monitorDefaultToNearest)
	}
	if mon == 0 {
		mon, _, _ = procMonitorFromPoint.Call(0, 0, monitorDefaultToPrimary)
	}
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return rect{0, 0, 1920, 1080}
	}
	return mi.Work
}

// fullscreenActive reports whether something is running full-screen, in which
// case the card stays away.
//
// Two checks, because neither alone is enough: the shell's own notification
// state catches exclusive-mode D3D and presentation mode, while the geometry
// check catches borderless-window games that the shell still considers
// ordinary windows.
func fullscreenActive() bool {
	if procSHQueryNotifyState.Find() == nil {
		var state int32
		if r, _, _ := procSHQueryNotifyState.Call(uintptr(unsafe.Pointer(&state))); r == 0 {
			switch state {
			case 2, // QUNS_BUSY: a full-screen app is running
				3, // QUNS_RUNNING_D3D_FULL_SCREEN
				4, // QUNS_PRESENTATION_MODE
				7: // QUNS_APP: full-screen store app
				return true
			}
		}
	}

	fg, _, _ := procGetForegroundWin.Call()
	if fg == 0 {
		return false
	}
	if visible, _, _ := procIsWindowVisible.Call(fg); visible == 0 {
		return false
	}
	switch windowClassName(fg) {
	case "Progman", "WorkerW", "Shell_TrayWnd", "Windows.UI.Core.CoreWindow":
		// Desktop, taskbar and shell surfaces cover the screen but are not
		// "a full-screen app".
		return false
	}

	var wr rect
	if r, _, _ := procGetWindowRect.Call(fg, uintptr(unsafe.Pointer(&wr))); r == 0 {
		return false
	}
	mon, _, _ := procMonitorFromWindow.Call(fg, monitorDefaultToNearest)
	if mon == 0 {
		return false
	}
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return false
	}
	m := mi.Monitor
	return wr.Left <= m.Left && wr.Top <= m.Top &&
		wr.Right >= m.Right && wr.Bottom >= m.Bottom
}

func windowClassName(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}
