package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// logWriter keeps the last stretch of what the adapter did, for a build that
// has no window to print to.
//
// A tray application is silent by design, which is fine right up to the moment
// something stops working — and then "it is not writing and I cannot see why"
// is the whole of what the operator can report. The file is what makes that
// answerable.
type logWriter struct {
	mu   sync.Mutex
	path string
}

// logCap is where the file is truncated. Small on purpose: this exists to
// answer "what happened just now", and an unbounded log on somebody's laptop
// is a slow leak nobody agreed to.
const logCap = 512 << 10

func newLogWriter() *logWriter {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "BurnRate")
	_ = os.MkdirAll(dir, 0o700)
	return &logWriter{path: filepath.Join(dir, "adapter.log")}
}

func (l *logWriter) Path() string { return l.path }

func (l *logWriter) Write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if fi, err := os.Stat(l.path); err == nil && fi.Size() > logCap {
		// Keep the newest half rather than deleting everything: a truncation
		// that throws away the moment a failure started makes the file
		// useless exactly when it is wanted.
		if b, err := os.ReadFile(l.path); err == nil {
			_ = os.WriteFile(l.path, b[len(b)/2:], 0o600)
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", stamp(), line)
}
