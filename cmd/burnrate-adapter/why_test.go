package main

import (
	"strings"
	"testing"
)

// The log is the only account of what the adapter did, so one failure must not
// be able to bury the rest of it. A proxy error page during a deploy put an
// entire HTML document into this file, one line per tag, which pushed the
// history that mattered out through the log's own truncation.
func TestAFailureIsOneShortLine(t *testing.T) {
	html := "<!doctype html><html><head><title>502</title></head><body>" +
		strings.Repeat("<path d=\"M63.6306 12.7658V5.24959H65.1883Z\"/>", 400) +
		"</body></html>"

	got := why([]byte(html))
	if len(got) > 210 {
		t.Errorf("a failed response produced %d characters of log", len(got))
	}
	if strings.Contains(got, "\n") {
		t.Error("the line contains newlines, so one failure spans many log lines")
	}
}

// BurnRate answers in JSON, and its own message is the useful part.
func TestAJSONErrorIsReportedAsItself(t *testing.T) {
	got := why([]byte(`{"error":"MoneyLover writing is switched off in BurnRate"}`))
	if got != "MoneyLover writing is switched off in BurnRate" {
		t.Errorf("got %q", got)
	}
}

func TestAnEmptyBodySaysSo(t *testing.T) {
	if got := why(nil); got == "" {
		t.Error("an empty body produced an empty reason, so the log line says nothing")
	}
}
