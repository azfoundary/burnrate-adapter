//go:build !windows

package main

import "context"

// launch falls back to the window everywhere without a tray yet.
//
// A tray on macOS and Linux needs CGO — Cocoa and GTK respectively — which
// would break the single-runner build that produces all four binaries today.
// Printing to a terminal is honest and works; pretending otherwise would mean
// shipping a macOS build that quietly does nothing in the background.
func launch(ctx context.Context, cfg adapterConfig, o loopOpts) error {
	return runLoop(ctx, cfg, o, newConsoleUI(cfg))
}
