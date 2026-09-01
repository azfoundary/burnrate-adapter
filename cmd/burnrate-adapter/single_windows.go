package main

import "golang.org/x/sys/windows"

// alreadyRunning reports whether another adapter owns this login session.
//
// Once it starts at login, double-clicking the binary is the obvious thing a
// person does when they want to check on it — and without this they get a
// second tray icon, a second poll loop, and two programs racing for the same
// queue. The server hands each row to one caller only, so nothing is written
// twice, but "which of these two icons is the real one" is not a question
// anybody should have to answer.
//
// Local\ scopes the mutex to this login session, so fast user switching gives
// each signed-in person their own adapter.
func alreadyRunning() bool {
	name, err := windows.UTF16PtrFromString(`Local\BurnRateAdapter`)
	if err != nil {
		return false
	}
	// The handle is deliberately never closed: it is released when the process
	// exits, which is exactly the lifetime being claimed.
	_, err = windows.CreateMutex(nil, false, name)
	return err == windows.ERROR_ALREADY_EXISTS
}
