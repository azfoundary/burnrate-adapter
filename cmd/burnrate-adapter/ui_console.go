package main

import (
	"fmt"
	"os"
	"time"
)

// consoleUI is the adapter as it has always looked: a window that says what it
// is doing. Kept for --console, --once and every platform without a tray.
type consoleUI struct{ started bool }

func newConsoleUI(cfg adapterConfig) *consoleUI {
	fmt.Printf("\n  BurnRate Adapter %s\n", adapterVersion)
	fmt.Printf("  Connected to %s\n\n", short(cfg.Server))
	return &consoleUI{}
}

// Set is deliberately quiet. A line every sixty seconds saying "still idle"
// buries the lines that matter; the states worth announcing announce
// themselves through Logf.
func (c *consoleUI) Set(state, string) {}

func (c *consoleUI) Wrote(n int) { fmt.Printf("  Wrote %d. Waiting.\n", n) }

func (c *consoleUI) Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
}

// stamp is the prefix on every line in the log file. The console does not use
// it: a live window is read as it happens, but a file is read hours later by
// somebody asking when something stopped.
func stamp() string { return time.Now().Format("2006-01-02 15:04:05") }
