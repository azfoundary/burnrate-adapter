package main

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/azfoundary/burnrate-adapter/moneylover"
)

// What BurnRate is told decides whether real money reaches the wallet, so the
// rule is pinned rather than left to the shape of a switch statement.
//
// "Landed" means the request may have arrived, so BurnRate holds the row for
// four hours instead of offering it again. Saying that about a write which
// never left this machine keeps the money out of the wallet AND hides the row
// from the only list that tracks it — while the heartbeat keeps the adapter
// looking healthy. That combination is why this test exists.
func TestOnlyWritesThatMayHaveArrivedAreReportedAsLanded(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		wantLanded bool
		wantOK     bool
	}{
		{
			name:       "a successful write landed",
			err:        nil,
			wantLanded: true,
			wantOK:     true,
		},
		{
			// The case that was reported as landed and must not be: with no
			// password the client refuses before it builds a request, which is
			// every run resumed from a cached session once the hour is up.
			name:       "an expired session never sent anything",
			err:        fmt.Errorf("%w: ML_EMAIL / ML_PASSWORD not configured", moneylover.ErrAuth),
			wantLanded: false,
		},
		{
			name:       "a bot challenge is Cloudflare answering, not MoneyLover",
			err:        moneylover.ErrBotChallenge,
			wantLanded: false,
		},
		{
			// Deliberately held: the request may have been received and
			// applied before the connection died. Writing it twice is worse
			// than holding it once.
			name:       "an ambiguous failure is held, not retried",
			err:        &net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
			wantLanded: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := classify(42, c.err)
			if got.TxnID != 42 {
				t.Errorf("txn id %d, want 42 — the server would settle the wrong row", got.TxnID)
			}
			if got.Landed != c.wantLanded {
				t.Errorf("Landed = %v, want %v", got.Landed, c.wantLanded)
			}
			if got.OK != c.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, c.wantOK)
			}
			if c.err != nil && got.Error == "" {
				t.Error("no reason reported, so the operator sees a failure with no cause")
			}
		})
	}
}

// A wrapped error must classify the same as a bare one: the write path returns
// these wrapped in context, and errors.Is is the only reason that works.
func TestWrappedErrorsClassifyTheSame(t *testing.T) {
	wrapped := fmt.Errorf("adding transaction: %w", moneylover.ErrAuth)
	if classify(7, wrapped).Landed {
		t.Error("a wrapped auth failure was reported as possibly landed")
	}
}
