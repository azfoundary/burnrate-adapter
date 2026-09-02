package main

import "testing"

// The tray asks this once a minute to label a menu item, so it must answer
// without unsealing the password: decrypting a credential to display
// something harmless is the wrong trade.
func TestSavedEmailAnswersWithoutThePassword(t *testing.T) {
	got := emailFrom([]byte(`{"email":"someone@example.com","password_sealed":"bm90LXJlYWRhYmxl"}`))
	if got != "someone@example.com" {
		t.Errorf("savedEmail parsed %q", got)
	}
}

func TestNoLoginReportsNoAccount(t *testing.T) {
	if got := emailFrom([]byte(`not json`)); got != "" {
		t.Errorf("unreadable file reported account %q", got)
	}
	if got := emailFrom(nil); got != "" {
		t.Errorf("missing file reported account %q", got)
	}
}
