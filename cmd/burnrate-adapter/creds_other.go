//go:build !windows

package main

// macOS and Linux store the password unsealed, in a file readable only by the
// owner.
//
// Stated plainly rather than dressed up: this is weaker than the Windows path,
// where the operating system ties the ciphertext to the signed-in account.
// Keychain and Secret Service would fix it and both want CGO, which would
// break the single cross-compiled build that produces all four binaries. Until
// that is solved, an operator on a Mac choosing to keep their login here
// should know it is a 0600 file and not much more.
func sealPassword(pw string) ([]byte, error)  { return []byte(pw), nil }
func unsealPassword(b []byte) (string, error) { return string(b), nil }
