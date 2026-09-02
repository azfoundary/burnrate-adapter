package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/azfoundary/burnrate-adapter/moneylover"
	"golang.org/x/term"
)

// runSignIn saves a MoneyLover login on this computer.
//
// Its own command rather than a dialog. A tray program has no window to type
// into, and building one means a GUI toolkit and CGO — so the tray launches
// this in a console instead, which is a small amount of plumbing against a
// large amount of dependency.
//
// The password is checked against MoneyLover before it is stored. Saving an
// unverified one produces the worst version of this: a tray that looks set up
// and fails every read afterwards, with the reason only in a log file.
func runSignIn(ctx context.Context) error {
	attachConsole()
	fmt.Println("\n  BurnRate Adapter — MoneyLover sign-in")
	fmt.Println("  This stays on this computer. BurnRate never receives it.")
	fmt.Println()

	email := promptLine("  MoneyLover email: ")
	password := promptSecret("  MoneyLover password: ")
	if email == "" || password == "" {
		return fmt.Errorf("both an email and a password are needed")
	}

	fmt.Println("\n  Checking with MoneyLover…")
	ml := moneylover.New(email, password, sessionCachePath())
	ml.SetSettings(localSettings{})
	if _, err := ml.ListWallets(ctx); err != nil {
		return fmt.Errorf("MoneyLover did not accept that: %w", err)
	}
	if err := saveCreds(email, password); err != nil {
		return fmt.Errorf("signed in, but could not save the login: %w", err)
	}
	fmt.Println("  Saved. The adapter can now read your wallet for BurnRate.")
	fmt.Printf("  Stored at %s\n", credsPath())
	holdOpen()
	return nil
}

// holdOpen waits for a keypress before the window closes.
//
// The tray opens this console itself, so it disappears the instant this
// returns - taking the outcome with it. A sign-in that says nothing before
// vanishing cannot be told apart from one that never ran.
func holdOpen() {
	fmt.Print("\n  Press Enter to close this window. ")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// runSignOut removes the saved login.
func runSignOut() error {
	attachConsole()
	if err := forgetCreds(); err != nil {
		return err
	}
	_ = os.Remove(sessionCachePath())
	fmt.Println("\n  The saved MoneyLover login has been removed from this computer.")
	fmt.Println("  BurnRate cannot read your wallet until you sign in again here,")
	fmt.Println("  or move the login back to BurnRate in Settings.")
	holdOpen()
	return nil
}

func sessionCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".burnrate-adapter-session.json"
	}
	return home + string(os.PathSeparator) + ".burnrate-adapter-session.json"
}

func promptLine(label string) string {
	fmt.Print(label)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

// promptSecret reads without echoing. A password typed onto a visible line
// ends up in screenshots and scrollback, which is exactly how one was exposed
// while this was being built.
func promptSecret(label string) string {
	fmt.Print(label)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return promptLine("")
	}
	return strings.TrimSpace(string(b))
}
