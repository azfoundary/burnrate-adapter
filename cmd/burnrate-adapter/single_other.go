//go:build !windows

package main

// A second console adapter is visibly a second window, so there is nothing to
// disambiguate and nothing to guard against.
func alreadyRunning() bool { return false }
