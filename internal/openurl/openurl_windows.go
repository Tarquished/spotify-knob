// Package openurl launches an external URI through the OS's own handler.
package openurl

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	shell32          = syscall.NewLazyDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteW")
)

const swShowNormal = 1

// Open hands uri to Windows exactly the way double-clicking a shortcut to it
// would: a "spotify:" URI goes to the desktop app if one is installed and
// registered the protocol, anything else goes to the default browser. There
// is no console to flash - the windowless build has none to flash into
// anyway - and the call does not wait for whatever it launched to exit.
func Open(uri string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(uri)
	if err != nil {
		return err
	}

	r, _, callErr := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0, 0,
		swShowNormal,
	)
	// ShellExecuteW returns a value > 32 on success; anything <= 32 is an
	// error code smuggled back as an HINSTANCE, per its own documentation.
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW(%q): code %d: %w", uri, r, callErr)
	}
	return nil
}
