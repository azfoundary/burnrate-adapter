package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// AttachConsole is not in x/sys/windows, so it is resolved by hand alongside
// AllocConsole rather than reaching for another dependency for two calls.
const attachParentProcess = ^uint32(0) // (DWORD)-1: the process that launched us

// attachConsole gives a windowsgui build somewhere to print.
//
// The Windows binary is linked with -H windowsgui so that double-clicking it
// opens no black window — which also means it has no stdout at all. Without
// this, --probe and --console would run correctly and print into nothing, and
// the operator would see an empty window and conclude the program is broken.
//
// Attaching to the terminal that launched it comes first, so output lands
// where the person is looking. Allocating a fresh console is the fallback for
// a double-click.
func attachConsole() {
	if !winConsoleCall("AttachConsole", uintptr(attachParentProcess)) {
		if !winConsoleCall("AllocConsole") {
			return
		}
	}
	// Go cached the handles at startup, when there was no console, so stdout
	// and stderr have to be reopened against the one that now exists.
	for _, f := range []**os.File{&os.Stdout, &os.Stderr} {
		if h, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			*f = h
		}
	}
}

// winConsoleCall reports whether a kernel32 console call succeeded.
func winConsoleCall(name string, args ...uintptr) bool {
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc(name).Call(args...)
	return r != 0
}
