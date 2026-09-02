package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// credsName sits beside the settings file, not inside it.
//
// Separate because they have different lives: burnrate-adapter.json is
// downloaded from BurnRate and replaced whenever a key is reissued, and a
// MoneyLover password that lived in it would be destroyed by an ordinary
// re-download.
const credsName = "burnrate-adapter-login.json"

// creds is the MoneyLover login, kept on this computer and nowhere else.
//
// Only written when the operator has chosen to keep it here rather than in
// BurnRate. The password is sealed by the operating system where the operating
// system offers it (see creds_windows.go), so the file alone is not enough on
// another machine.
type creds struct {
	Email  string `json:"email"`
	Sealed string `json:"password_sealed"` // base64 of sealPassword's output
}

var errNoCreds = errors.New("no MoneyLover login is saved on this computer")

func credsPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), credsName)
	}
	return credsName
}

// loadCreds returns the saved login, or errNoCreds.
func loadCreds() (email, password string, err error) {
	b, err := os.ReadFile(credsPath())
	if err != nil {
		return "", "", errNoCreds
	}
	var c creds
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", fmt.Errorf("the saved MoneyLover login is not readable: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(c.Sealed)
	if err != nil {
		return "", "", fmt.Errorf("the saved MoneyLover login is not readable: %w", err)
	}
	pw, err := unsealPassword(raw)
	if err != nil {
		// Sealed to a different Windows account or a different machine. Saying
		// so is the whole point: "wrong password" would send the operator to
		// MoneyLover to reset a password that is perfectly correct.
		return "", "", fmt.Errorf("the saved MoneyLover login cannot be unlocked on this computer or account: %w", err)
	}
	if c.Email == "" || pw == "" {
		return "", "", errNoCreds
	}
	return c.Email, pw, nil
}

// savedEmail is which MoneyLover account is signed in here, or "".
//
// Reads the file WITHOUT unsealing the password: the tray asks this once a
// minute merely to label a menu item, and decrypting a credential to answer
// "are we signed in" would be doing something dangerous to display something
// harmless.
func savedEmail() string {
	b, err := os.ReadFile(credsPath())
	if err != nil {
		return ""
	}
	return emailFrom(b)
}

// emailFrom is the parsing half, split out so it can be tested without a file
// beside an executable a test cannot place.
func emailFrom(b []byte) string {
	var c creds
	if err := json.Unmarshal(b, &c); err != nil {
		return ""
	}
	return c.Email
}

func saveCreds(email, password string) error {
	sealed, err := sealPassword(password)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(creds{
		Email:  email,
		Sealed: base64.StdEncoding.EncodeToString(sealed),
	}, "", "  ")
	if err != nil {
		return err
	}
	// 0600 regardless of sealing. On Windows it is close to decorative and the
	// sealing is what protects the file; everywhere else it is the only thing
	// that does.
	return os.WriteFile(credsPath(), b, 0o600)
}

func forgetCreds() error {
	if err := os.Remove(credsPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
