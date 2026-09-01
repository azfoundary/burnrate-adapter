package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows seals the password with DPAPI, tied to the signed-in account.
//
// So the file on disk is not the password: copied to another machine, or read
// by another account on this one, it does not decrypt. That matters because
// the whole reason for keeping the login here rather than in BurnRate is that
// it should be harder to get at, not merely somewhere else.
//
// CryptProtectData is not in x/sys/windows, so it is resolved by hand rather
// than taking a dependency for two calls.
var (
	crypt32          = windows.NewLazySystemDLL("crypt32.dll")
	procProtectData  = crypt32.NewProc("CryptProtectData")
	procUnprotectDat = crypt32.NewProc("CryptUnprotectData")
	kernel32Local    = windows.NewLazySystemDLL("kernel32.dll")
	procLocalFree    = kernel32Local.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (b dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// cryptprotectUIForbidden: never put a prompt on screen. This runs from a tray
// program with no window, where a modal nobody expected is worse than an error.
const cryptprotectUIForbidden = 0x1

func sealPassword(pw string) ([]byte, error) {
	in := newBlob([]byte(pw))
	var out dataBlob
	r, _, err := procProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(cryptprotectUIForbidden), uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("Windows would not protect the saved password: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func unsealPassword(sealed []byte) (string, error) {
	in := newBlob(sealed)
	var out dataBlob
	r, _, err := procUnprotectDat.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(cryptprotectUIForbidden), uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return "", err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return string(out.bytes()), nil
}
