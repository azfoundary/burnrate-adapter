# BurnRate Adapter

Writes confirmed expenses into your MoneyLover wallet from a computer you own.

## Why this exists

MoneyLover has no public API. The private endpoint their own apps use sits
behind bot protection that **refuses requests from servers**: a write from a
datacentre gets a Cloudflare challenge and never reaches MoneyLover at all,
while the same request from a personal computer is accepted and answered
normally. Reads are unaffected.

So [BurnRate](https://github.com/azfoundary/burnrate) reads your wallet on its
own, and hands the *writing* to this program, which runs where you are.

## What it does, and does not

It asks your BurnRate instance what to write, sends exactly that to MoneyLover,
and reports back what happened.

Every decision stays on the server — which rows, which wallet, which category,
what the note says. This program is handed finished payloads it can only
transmit. **A bug here can fail to write; it cannot write the wrong thing.**

**This program holds no credentials at all.** Your MoneyLover login stays in
BurnRate, which needs it anyway in order to read your wallet. Each batch of
work arrives with a short-lived MoneyLover session attached, used for those
writes and then dropped. Nothing is stored, nothing is cached, and there is
nothing to type.

## Using it

1. Download the adapter for your platform from
   [Releases](https://github.com/azfoundary/burnrate-adapter/releases/latest):

   | Platform | File |
   | --- | --- |
   | macOS (Apple Silicon) | `burnrate-adapter-darwin-arm64` |
   | macOS (Intel) | `burnrate-adapter-darwin-amd64` |
   | Windows | `burnrate-adapter-windows-amd64.exe` |
   | Linux | `burnrate-adapter-linux-amd64` |

   A browser cannot tell an Apple Silicon Mac from an Intel one, so if the
   file refuses to open and mentions the CPU, you have the other one.
2. In your own BurnRate, go to **Settings ▸ Adapter** and download
   `burnrate-adapter.json`. It already contains your address and key.
3. Put both downloads — the file from step 1 and `burnrate-adapter.json`
   from step 2 — in the **same folder**, then run the adapter. It looks for
   its settings beside itself, so the folder is the only thing that matters.

It asks for nothing. It checks for work every 60 seconds, and each batch
arrives with the session it needs.

**On Windows** it runs in the notification area, by the clock. No window. The
icon's colour is the status — green reaching BurnRate, amber BurnRate has
paused writing, red cannot reach it — and right-clicking gives you *Check for
work now*, *Start when I log in*, *Show log* and *Quit*.

**On macOS and Linux** it is still a terminal window that has to stay open. A
tray on those needs a different toolkit each, and both need CGO, which would
break the single cross-compiled build that produces all four binaries.

Either way it only writes while it is running and the computer is awake.
Quitting it loses nothing: confirmed rows queue in BurnRate until it is back.

### Running it in a window anyway

    burnrate-adapter --console      # print to this terminal instead of the tray
    burnrate-adapter --once         # one pass, then stop

On Windows these attach to the terminal you launched them from. The binary is
linked as a GUI application so that double-clicking it opens no black window,
which also means it has no console of its own until it asks for one.

MoneyLover sessions last about an hour and there is no way to renew one without
the password, which is never stored. So the adapter asks you to sign in again
when it lapses. If it is running in a window it asks there and carries on; if it
was started at login with no window, it stops and says so, rather than staying
up looking healthy while nothing can be written.

**Your computer will warn about an unsigned download the first time.** On macOS
right-click the file and choose Open, then Open again. On Windows click
**More info**, then **Run anyway**. Signing removes these and costs a few
hundred dollars a year, which is not yet worth it.

### Checking before committing to it

    burnrate-adapter --probe

Sends a deliberately invalid write and reports which side answered. MoneyLover
rejecting it is the good outcome — it means the request arrived. Nothing is
created either way. It borrows a session from BurnRate to do this, so BurnRate
has to be reachable and its MoneyLover login has to work.

### While it is not running

Confirmed expenses queue up and wait. Nothing is lost and nothing is written,
and BurnRate says so on the page where the rows are waiting rather than letting
a queue look like progress.

## What is deliberately not here

No `cf_clearance` cookie handling, no headless-browser challenge solving, no
fingerprint spoofing. The protection is doing its job; this program works
because it runs somewhere requests are accepted on their own merits, not
because it pretends to be something it is not.

## Adding another service

MoneyLover is not unusual — plenty of consumer tools have no API and no plans
for one. The shape here is meant to survive that: the adapter's own half (the
queue protocol, the config file, the heartbeat, the three-way reporting of what
happened to each row) knows nothing about MoneyLover, and everything that does
lives in `moneylover/`.

What is deliberately **not** here is an interface with one implementation. A
second service is what reveals where the seam actually belongs; guessing at it
from one produces a shape that fits nothing. The config file carries a
`service` field so that files already sitting in people's Downloads folders can
be told apart later, and that is the whole of the preparation.

## Licence

MIT.
