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
	// Both of these fail when the process already has a console — which is the
	// case for the sign-in window the tray spawns with CREATE_NEW_CONSOLE.
	// That is not an error, and it must not skip the reopening below: it did,
	// and the sign-in prompt then had nowhere to read from.
	if !winConsoleCall("AttachConsole", uintptr(attachParentProcess)) {
		winConsoleCall("AllocConsole")
	}
	// Go cached the std handles at startup, when a windowsgui process had
	// none. Reopen all three against whatever console exists now.
	//
	// STDIN especially. A child started by the tray inherits NUL for stdin,
	// because exec.Command with a nil Stdin hands the process the null device
	// on Windows — so every prompt read end-of-file the instant it was asked,
	// and signing in failed with "both an email and a password are needed"
	// while a console window sat there with nowhere to type.
	if h, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = h
	}
	for _, f := range []**os.File{&os.Stdout, &os.Stderr} {
		if h, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
			*f = h
		}
	}
}

// winConsoleCall reports whether a kernel32 console call succeeded.
func winConsoleCall(name string, args ...uintptr) bool {
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc(name).Call(args...)
	return r != 0
}
