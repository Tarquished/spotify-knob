// Package hotkey captures the keyboard's media keys with a low-level Windows
// keyboard hook (WH_KEYBOARD_LL) and swallows them, so the knob drives Spotify
// instead of the Windows master volume.
//
// This replaces the AutoHotkey layer from the original design: the hook lives
// in the same process as the daemon, so a knob turn never has to cross a
// localhost HTTP hop. The hook callback must return fast (Windows drops hooks
// that take longer than LowLevelHooksTimeout, ~300ms), so it only does a
// non-blocking send on a buffered channel.
package hotkey

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Event is a physical knob action.
type Event int

const (
	VolumeUp Event = iota
	VolumeDown
	Press   // knob pushed down
	PressUp // knob released
)

func (e Event) String() string {
	switch e {
	case VolumeUp:
		return "volume_up"
	case VolumeDown:
		return "volume_down"
	case Press:
		return "press"
	case PressUp:
		return "press_up"
	}
	return "unknown"
}

const (
	whKeyboardLL = 13
	hcAction     = 0

	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105
	wmQuit       = 0x0012
	wmTimer      = 0x0113

	// threadPriorityHighest keeps the hook thread ahead of ordinary work.
	// The callback has to return inside LowLevelHooksTimeout or Windows drops
	// the hook, and a busy machine is exactly when that matters.
	threadPriorityHighest = 2

	// reinstallEvery re-registers the hook on a timer. Windows removes a
	// low-level hook whose callback ran late, silently and with no way to
	// query it, so the only reliable recovery is to put it back periodically.
	reinstallEvery = 20 * time.Second

	vkShift      = 0x10
	vkControl    = 0x11
	vkMenu       = 0x12 // Alt
	vkVolumeMute = 0xAD
	vkVolumeDown = 0xAE
	vkVolumeUp   = 0xAF
)

type kbdllhookstruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procSetWindowsHookEx = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHex = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx   = user32.NewProc("CallNextHookEx")
	procGetMessage       = user32.NewProc("GetMessageW")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	procPostThreadMsg    = user32.NewProc("PostThreadMessageW")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")

	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandle   = kernel32.NewProc("GetModuleHandleW")
	procGetCurrentThread  = kernel32.NewProc("GetCurrentThreadId")
	procGetCurrentThreadH = kernel32.NewProc("GetCurrentThread")
	procSetThreadPriority = kernel32.NewProc("SetThreadPriority")
)

// sink is set while a Hook is running. The hook callback is a package-level
// function, so it needs package-level state to reach the channel.
var sink atomic.Pointer[chan Event]

// Hook installs the keyboard hook and emits Events.
type Hook struct {
	log        *slog.Logger
	events     chan Event
	ready      chan struct{}
	threadID   atomic.Uint32
	dropped    atomic.Uint64
	reinstalls atomic.Uint64
}

func New(log *slog.Logger) *Hook {
	return &Hook{
		log: log,
		// Generous buffer: a fast knob spin is bursty, and the consumer only
		// touches in-memory state per event.
		events: make(chan Event, 256),
		ready:  make(chan struct{}),
	}
}

// Ready closes once the hook is installed and keys are actually being seen.
// Without it, anything that needs the hook live has to guess with a sleep,
// which is exactly the kind of timing assumption that fails under load.
func (h *Hook) Ready() <-chan struct{} { return h.ready }

// Events yields knob actions. The channel is closed when Run returns.
func (h *Hook) Events() <-chan Event { return h.events }

// Dropped counts events lost because the consumer fell behind.
func (h *Hook) Dropped() uint64 { return h.dropped.Load() }

// Reinstalls counts how many times the hook has been put back.
func (h *Hook) Reinstalls() uint64 { return h.reinstalls.Load() }

// Run installs the hook and pumps the Windows message loop until ctx is
// cancelled. It must own its OS thread: a low-level hook is delivered to the
// thread that installed it, and only if that thread pumps messages.
func (h *Hook) Run(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(h.events)

	ch := h.events
	sink.Store(&ch)
	defer sink.Store(nil)
	hookOwner = h

	tid, _, _ := procGetCurrentThread.Call()
	h.threadID.Store(uint32(tid))

	// Input handling is latency-critical: if this thread is descheduled long
	// enough for the callback to miss its deadline, the hook is removed.
	if th, _, _ := procGetCurrentThreadH.Call(); th != 0 {
		procSetThreadPriority.Call(th, threadPriorityHighest)
	}

	mod, _, _ := procGetModuleHandle.Call(0)
	install := func() (uintptr, error) {
		handle, _, err := procSetWindowsHookEx.Call(
			uintptr(whKeyboardLL),
			syscall.NewCallback(hookProc),
			mod,
			0,
		)
		if handle == 0 {
			return 0, fmt.Errorf("SetWindowsHookExW: %w", err)
		}
		return handle, nil
	}

	handle, err := install()
	if err != nil {
		return err
	}
	defer func() { procUnhookWindowsHex.Call(handle) }()
	close(h.ready)
	h.log.Info("keyboard hook installed", "keys", "VolumeUp/VolumeDown/VolumeMute")

	// Re-register on a timer. There is no way to ask Windows whether the hook
	// is still live, so it is simply put back often enough that a silent
	// removal costs at most one interval of a dead knob.
	timerID, _, _ := procSetTimer.Call(0, 0, uintptr(reinstallEvery/time.Millisecond), 0)
	if timerID != 0 {
		defer procKillTimer.Call(0, timerID)
	}

	// Cancelling the context has to break GetMessageW, which blocks. A posted
	// WM_QUIT is the documented way out.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			procPostThreadMsg.Call(uintptr(h.threadID.Load()), wmQuit, 0, 0)
		case <-stop:
		}
	}()

	var msg struct {
		HWND    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      struct{ X, Y int32 }
	}
	for {
		r, _, err := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(r) {
		case 0: // WM_QUIT
			h.log.Info("keyboard hook stopped")
			return nil
		case -1:
			return fmt.Errorf("GetMessageW: %w", err)
		}
		if msg.Message == wmTimer {
			procUnhookWindowsHex.Call(handle)
			next, ierr := install()
			if ierr != nil {
				h.log.Error("could not re-register the keyboard hook", "err", ierr)
				continue
			}
			handle = next
			h.reinstalls.Add(1)
			h.log.Debug("keyboard hook re-registered")
		}
	}
}

// AltPressed reports whether Alt is held right now.
//
// The knob has one button, and holding it is not available to us: the
// keyboard's own firmware claims a long hold for its lighting menu before
// Windows ever sees an event. Alt is the next best modifier - Shift is
// already the pass-through escape hatch.
func AltPressed() bool {
	state, _, _ := procGetAsyncKeyState.Call(vkMenu)
	return state&0x8000 != 0
}

// CtrlPressed reports whether Ctrl is held right now. It is the modifier for
// the lyrics window, chosen because it is the one combination the knob's own
// firmware leaves alone and Alt is already spoken for by the queue peek.
func CtrlPressed() bool {
	state, _, _ := procGetAsyncKeyState.Call(vkControl)
	return state&0x8000 != 0
}

// hookOwner lets the callback report drops. Only one Hook runs at a time.
var hookOwner *Hook

// mutePressed tracks the knob's physical state. Only the hook thread touches
// it, so it needs no synchronisation.
var mutePressed bool

// hookProc runs on the hook thread for every key event system-wide. Keep it
// cheap and allocation-free.
func hookProc(nCode uintptr, wParam uintptr, lParam uintptr) uintptr {
	if int32(nCode) != hcAction {
		return callNext(nCode, wParam, lParam)
	}

	info := (*kbdllhookstruct)(unsafe.Pointer(lParam))
	switch info.VkCode {
	case vkVolumeUp, vkVolumeDown, vkVolumeMute:
	default:
		return callNext(nCode, wParam, lParam)
	}

	// Escape hatch: hold Shift to let the key through to Windows, so the knob
	// can still drive the master volume when needed.
	if state, _, _ := procGetAsyncKeyState.Call(vkShift); state&0x8000 != 0 {
		return callNext(nCode, wParam, lParam)
	}

	down := wParam == wmKeyDown || wParam == wmSysKeyDown

	// Rotation only reports on the down edge; the press reports both, because
	// holding the knob is its own gesture and that needs a release to close.
	emit := func(ev Event) {
		if chp := sink.Load(); chp != nil {
			select {
			case *chp <- ev:
			default:
				if hookOwner != nil {
					hookOwner.dropped.Add(1)
				}
			}
		}
	}

	switch {
	case info.VkCode == vkVolumeMute:
		if down {
			// Media keys do not auto-repeat, but guard anyway: a repeat would
			// otherwise look like a second press.
			if !mutePressed {
				mutePressed = true
				emit(Press)
			}
		} else if mutePressed {
			mutePressed = false
			emit(PressUp)
		}
	case down && info.VkCode == vkVolumeUp:
		emit(VolumeUp)
	case down && info.VkCode == vkVolumeDown:
		emit(VolumeDown)
	}

	// Swallow both the down and the up edge so Windows never sees the key.
	return 1
}

func callNext(nCode, wParam, lParam uintptr) uintptr {
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}
