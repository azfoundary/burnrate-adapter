package main

import "golang.org/x/sys/windows"

// reportFatal shows a startup failure in a dialog box.
//
// The Windows binary is linked -H windowsgui, so it has no stdout: a
// double-click with the settings file missing printed a carefully worded
// explanation into a dead handle and exited. No window, no tray icon, no
// dialog, and no log file either, because the log is only opened once the
// tray is up. The operator saw the cursor flicker and nothing else — with no
// way, inside the product, to find out why.
//
// That is the single likeliest first run: the exe on the Desktop and the
// settings file still in Downloads.
func reportFatal(msg string) {
	title, err := windows.UTF16PtrFromString("BurnRate Adapter")
	if err != nil {
		return
	}
	body, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	// MB_ICONERROR | MB_SETFOREGROUND: a background program's error box that
	// opens behind other windows has not told anybody anything.
	_, _ = windows.MessageBox(0, body, title, 0x00000010|0x00010000)
}
