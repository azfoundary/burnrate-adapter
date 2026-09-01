//go:build !windows

package main

import "errors"

// Autostart is Windows-only for now. macOS wants a LaunchAgent plist and Linux
// a systemd user unit; neither is a shortcut of the other, and guessing at a
// shared shape from one implementation produces the wrong seam.
func autostartEnabled() bool { return false }

func setAutostart(bool) error {
	return errors.New("starting at login is only available on Windows so far")
}
