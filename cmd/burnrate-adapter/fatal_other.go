//go:build !windows

package main

import (
	"fmt"
	"os"
)

// Everywhere else the process still has the terminal it was started from, so
// the message reaches somebody without a dialog.
func reportFatal(msg string) { fmt.Fprintln(os.Stderr, "burnrate-adapter: "+msg) }
