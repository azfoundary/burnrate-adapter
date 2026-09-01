//go:build !windows

package main

// attachConsole is a Windows problem. Everywhere else the process already has
// the terminal it was started from.
func attachConsole() {}
