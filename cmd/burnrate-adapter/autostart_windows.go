package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// The per-user Run key, not the machine-wide one under HKLM.
//
// HKLM would need the adapter to run elevated to install itself, and would
// start it for every account on the computer using one person's BurnRate key.
// This is one operator's own background program: it belongs to their login.
const (
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue   = "BurnRateAdapter"
	autoMarker = "--autostart"
)

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	if err != nil || v == "" {
		return false
	}
	// The entry has to point at THIS binary, not merely exist.
	//
	// Windows fails a Run entry whose target has moved without telling
	// anybody. Reporting "enabled" on the strength of a stale path meant the
	// menu showed a tick, nothing started at login, and the one control that
	// could have revealed it agreed with the operator that all was well.
	// Comparing means a moved binary shows unticked, and ticking it again
	// repairs the entry.
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(v), strings.ToLower(exe))
}

// setAutostart adds or removes the login entry.
//
// The stored command is the binary's CURRENT path, so moving the folder breaks
// it — deliberately visible rather than silently repaired, because a login
// entry pointing at a program that is no longer there is exactly the kind of
// thing that should be re-ticked by a person who knows where they moved it.
func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening your Windows startup list: %w", err)
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("removing the startup entry: %w", err)
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding where this program lives: %w", err)
	}
	// Quoted: the usual home for this is a Downloads folder, and an unquoted
	// path with a space in it is run as a different, missing program.
	if err := k.SetStringValue(runValue, `"`+exe+`" `+autoMarker); err != nil {
		return fmt.Errorf("adding the startup entry: %w", err)
	}
	return nil
}
