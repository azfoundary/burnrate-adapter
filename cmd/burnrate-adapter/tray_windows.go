package main

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
)

// trayUI is the adapter as a background program: an icon whose colour says
// whether writes are getting through, and a menu for the four things a person
// ever wants to do to it.
//
// The loop is unchanged and unaware. It reports through ui, and this decides
// how to show it — so there is still exactly one implementation of when a row
// may be written.
type trayUI struct {
	mu      sync.Mutex
	log     *logWriter
	status  *systray.MenuItem
	written int
	last    state
}

func (t *trayUI) Set(s state, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Idle and working alternate every sixty seconds. Repainting the icon each
	// time makes a tray that flickers all day, so only real changes are drawn.
	if s != t.last {
		systray.SetIcon(trayIcon(s))
		t.last = s
	}
	line := detail
	if t.written > 0 {
		line = fmt.Sprintf("%s · %d written", detail, t.written)
	}
	systray.SetTooltip("BurnRate Adapter — " + line)
	t.status.SetTitle(line)
}

func (t *trayUI) Wrote(n int) {
	t.mu.Lock()
	t.written += n
	t.mu.Unlock()
	t.Logf("wrote %d", n)
}

func (t *trayUI) Logf(format string, args ...any) {
	t.log.Write(fmt.Sprintf(format, args...))
}

// launch runs the adapter in the notification area.
//
// systray.Run must own the main thread — it is a Windows message loop — so the
// adapter runs beside it and Quit closes stop, which ends the loop cleanly
// rather than killing the process mid-batch. A batch cut in half would leave
// rows marked in flight that were never written.
func launch(ctx context.Context, cfg adapterConfig, o loopOpts) error {
	stop := make(chan struct{})
	now := make(chan struct{}, 1)
	o.now, o.stop = now, stop

	log := newLogWriter()
	var loopErr error
	var wg sync.WaitGroup

	// Quit is reached from the menu AND from the loop ending on its own, and
	// stop is closed on either path, so both need guarding. sync.Once is the
	// wrong guard for Quit: Do holds its mutex for the whole call, and
	// systray's Windows Quit runs onExit INLINE on the calling goroutine — so
	// the loop goroutine's own quit() would block on the Once the menu
	// goroutine was still inside, its deferred wg.Done() would never run, and
	// onExit's wait for the batch would deadlock until it timed out. Every
	// time. A non-blocking swap says "someone is already quitting" and returns.
	var quitting atomic.Bool
	var stopOnce sync.Once
	quit := func() {
		if quitting.CompareAndSwap(false, true) {
			systray.Quit()
		}
	}

	onReady := func() {
		systray.SetIcon(trayIcon(stateOffline))
		systray.SetTitle("BurnRate Adapter")
		systray.SetTooltip("BurnRate Adapter — starting")

		status := systray.AddMenuItem("Starting…", "")
		status.Disable()
		systray.AddSeparator()
		writeNow := systray.AddMenuItem("Check for work now", "Look for entries to write without waiting")
		open := systray.AddMenuItem("Open BurnRate", "Open the ledger in your browser")
		systray.AddSeparator()
		atLogin := systray.AddMenuItemCheckbox("Start when I log in", "Run the adapter automatically", autostartEnabled())
		showLog := systray.AddMenuItem("Show log", "Open the file recording what the adapter has done")
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit", "Stop writing to MoneyLover until started again")

		ui := &trayUI{log: log, status: status, last: -1}
		ui.Logf("BurnRate Adapter %s starting, connected to %s", adapterVersion, short(cfg.Server))

		wg.Add(1)
		go func() {
			defer wg.Done()
			loopErr = runLoop(ctx, cfg, o, ui)
			quit()
		}()

		go func() {
			for {
				select {
				case <-writeNow.ClickedCh:
					select {
					case now <- struct{}{}:
					default: // a pass is already due; asking twice changes nothing
					}
				case <-open.ClickedCh:
					openInBrowser(cfg.Server)
				case <-atLogin.ClickedCh:
					want := !atLogin.Checked()
					if err := setAutostart(want); err != nil {
						ui.Logf("could not change the startup setting: %v", err)
						continue
					}
					if want {
						atLogin.Check()
					} else {
						atLogin.Uncheck()
					}
				case <-showLog.ClickedCh:
					openInBrowser(log.Path())
				case <-quitItem.ClickedCh:
					quit()
					return
				}
			}
		}()
	}

	onExit := func() { stopOnce.Do(func() { close(stop) }) }

	systray.Run(onReady, onExit)

	// The batch is settled HERE, not in onExit.
	//
	// systray's Windows Quit posts WM_CLOSE and then runs onExit inline on the
	// goroutine that called it, while the main thread unwinds the message loop
	// independently — so a wait inside onExit does not hold up the process at
	// all. This does: main returns through here.
	//
	// Worth waiting for. BurnRate marks every row in flight before handing it
	// over, and a row written to the wallet but never reported stays held and
	// off the To-mirror list until a later pull settles it. The window covers
	// one batch of writes plus the report that follows.
	stopOnce.Do(func() { close(stop) })
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(settleWindow):
		log.Write("quit while a batch was still running; those rows settle on the next MoneyLover sync")
	}
	return loopErr
}

// settleWindow is how long Quit waits for a batch to finish reporting.
//
// Sized against what a batch can actually take: up to ten rows at the
// MoneyLover client's 90-second per-write ceiling, then the 30-second report.
// The old twenty seconds was shorter than a single slow write, so the wait
// looked like a safeguard while reliably expiring.
const settleWindow = 2 * time.Minute

func openInBrowser(target string) {
	if target == "" {
		return
	}
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}
