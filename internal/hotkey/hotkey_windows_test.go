package hotkey

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

const keyeventfKeyup = 0x0002

// requireNoDaemon skips when a spotify-knob daemon is already running.
//
// Two low-level hooks that both swallow the same keys means only one of them
// ever sees a given press - whichever sits at the head of the chain. With the
// daemon up, these tests fail in a way that looks like a hook bug and is not
// one, so say so plainly instead.
func requireNoDaemon(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:8888", 300*time.Millisecond)
	if err != nil {
		return
	}
	c.Close()
	t.Skip("a spotify-knob daemon is running; its keyboard hook would swallow these keys")
}

// waitReady blocks until the hook is installed, instead of sleeping and
// hoping. Under a parallel test run the fixed sleep was not always enough.
func waitReady(t *testing.T, h *Hook) {
	t.Helper()
	select {
	case <-h.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("keyboard hook never became ready")
	}
	// The hook is live; let the injection queue settle behind it.
	time.Sleep(50 * time.Millisecond)
}

var procKeybdEvent = user32.NewProc("keybd_event")

// tapKey synthesises a key press. The hook swallows it, so the machine's own
// volume is not touched by this test.
func tapKey(vk uintptr) {
	procKeybdEvent.Call(vk, 0, 0, 0)
	procKeybdEvent.Call(vk, 0, keyeventfKeyup, 0)
}

// The hook must see the media keys the knob sends. Rotation reports once, on
// the down edge; the press reports both edges, because holding the knob is a
// gesture of its own and needs a release to close it.
func TestHookCapturesMediaKeys(t *testing.T) {
	requireNoDaemon(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- h.Run(ctx) }()

	waitReady(t, h)

	tapKey(vkVolumeUp)
	tapKey(vkVolumeDown)
	tapKey(vkVolumeMute)

	want := []Event{VolumeUp, VolumeDown, Press, PressUp}
	for i, w := range want {
		select {
		case got := <-h.Events():
			if got != w {
				t.Fatalf("event %d: got %v, want %v", i, got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d (%v)", i, w)
		}
	}

	select {
	case extra := <-h.Events():
		t.Fatalf("unexpected extra event %v", extra)
	case <-time.After(200 * time.Millisecond):
	}

	if n := h.Dropped(); n != 0 {
		t.Fatalf("dropped %d events", n)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not shut down on context cancel")
	}
}

// Keys we do not own must pass through untouched.
// A held knob must produce exactly one Press, not a stream of them, even if
// the keyboard were to auto-repeat.
func TestHeldKnobReportsOnePress(t *testing.T) {
	requireNoDaemon(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	waitReady(t, h)

	procKeybdEvent.Call(vkVolumeMute, 0, 0, 0)
	procKeybdEvent.Call(vkVolumeMute, 0, 0, 0) // repeat while held
	procKeybdEvent.Call(vkVolumeMute, 0, 0, 0)
	time.Sleep(150 * time.Millisecond)
	procKeybdEvent.Call(vkVolumeMute, 0, keyeventfKeyup, 0)

	got := []Event{}
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev := <-h.Events():
			got = append(got, ev)
			if len(got) == 2 {
				break collect
			}
		case <-deadline:
			t.Fatalf("timed out, got %v", got)
		}
	}
	if got[0] != Press || got[1] != PressUp {
		t.Fatalf("want one Press then one PressUp, got %v", got)
	}
}

func TestHookIgnoresOtherKeys(t *testing.T) {
	requireNoDaemon(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	waitReady(t, h)

	const vkF24 = 0x87 // harmless: no default action anywhere
	tapKey(vkF24)

	select {
	case ev := <-h.Events():
		t.Fatalf("F24 should not produce an event, got %v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// Windows removes a low-level hook whose callback ran late, silently and with
// no way to query it. The only reliable recovery is to put the hook back on a
// timer, so the timer has to actually fire and keys must keep arriving after
// it does.
func TestHookSurvivesReinstallation(t *testing.T) {
	requireNoDaemon(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	waitReady(t, h)

	if h.Reinstalls() != 0 {
		t.Fatalf("no re-registration should have happened yet, got %d", h.Reinstalls())
	}

	// Force the re-registration path rather than waiting out the real
	// interval, then confirm keys still land.
	postThreadTimer(t, h)
	deadline := time.Now().Add(3 * time.Second)
	for h.Reinstalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.Reinstalls() == 0 {
		t.Fatal("the hook was never re-registered")
	}

	tapKey(vkVolumeUp)
	select {
	case ev := <-h.Events():
		if ev != VolumeUp {
			t.Fatalf("want VolumeUp after re-registration, got %v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("keys stopped arriving after the hook was re-registered")
	}
}

// postThreadTimer delivers a WM_TIMER to the hook thread, which is what the
// real timer does.
func postThreadTimer(t *testing.T, h *Hook) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.threadID.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.threadID.Load() == 0 {
		t.Fatal("hook thread never reported its id")
	}
	procPostThreadMsg.Call(uintptr(h.threadID.Load()), wmTimer, 0, 0)
}
