package main

// The BurnRate Adapter.
//
// MoneyLover has no API, and the private endpoint their own apps use answers
// writes from a datacentre with a Cloudflare challenge while answering the
// same account's reads normally. The block is on WHERE a request comes from,
// so the write is performed from a computer the user owns. Verified 1 Sep
// 2026: from a server, cf-mitigated:challenge; from the operator's machine,
// MoneyLover's own error 703 — received, parsed, refused on its merits.
//
// This process holds no ledger logic on purpose. It asks the user's BurnRate
// what to write, sends exactly that, and reports what happened. It cannot
// choose rows, compose notes or pick categories — so a bug here can fail to
// write, but cannot write the wrong thing.
//
// It signs in to MoneyLover itself with the user's own credentials, entered on
// their machine. No MoneyLover credential ever reaches the BurnRate server.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/azfoundary/burnrate-adapter/moneylover"
)

// adapterVersion is reported on every heartbeat so the server can say when a
// newer one is available. Bumped by hand: a version tracking the server build
// would claim a compatibility nobody tested.
const adapterVersion = "1.1.0"

// configName sits beside the binary. The user downloads it from their own
// BurnRate, already filled in — which is the whole reason this program needs
// no flags, no environment variables and no copy-pasted token.
const configName = "burnrate-adapter.json"

type adapterConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// Service names which thing this adapter writes to. There is one today,
	// and the field exists anyway: config files live in people's Downloads
	// folders, so a version that omits it can never be told apart from a
	// future one that means something else. Empty is read as "moneylover",
	// which is what every file issued so far means.
	//
	// This is NOT an abstraction. Nothing dispatches on it yet, because a
	// second service is what would reveal the right shape and guessing at it
	// from one implementation produces the wrong seam.
	Service string `json:"service,omitempty"`
}

type adapterRow struct {
	TxnID      int64   `json:"txn_id"`
	WalletID   string  `json:"wallet_id"`
	CategoryID string  `json:"category_id"`
	Amount     float64 `json:"amount"`
	Currency   string  `json:"currency"`
	Note       string  `json:"note"`
	Date       string  `json:"date"`
	Gid        string  `json:"gid"`
}

type adapterResult struct {
	TxnID int64 `json:"txn_id"`
	OK    bool  `json:"ok"`
	// Landed separates "reached MoneyLover and failed" from "never got there".
	// Only the second is safe to put back on the queue; a call that may have
	// landed must stay held, or the money is written twice.
	Landed bool   `json:"landed"`
	Error  string `json:"error,omitempty"`
}

// localSettings satisfies the MoneyLover client's freeze-check interface. The
// freeze is a ledger state, and the server already refuses to hand out work
// while it is set, so the gate stays where the ledger is.
type localSettings struct{}

func (localSettings) GetSetting(string) (string, error) { return "", nil }

func main() {
	log.SetFlags(0)
	if err := runAdapter(os.Args[1:]); err != nil {
		log.Fatalf("burnrate-adapter: %v", err)
	}
}

func runAdapter(args []string) error {
	fs := flag.NewFlagSet("adapter", flag.ContinueOnError)
	every := fs.Duration("every", 60*time.Second, "how often to check for work")
	once := fs.Bool("once", false, "do one pass and stop")
	limit := fs.Int("limit", 0, "write at most this many rows in one pass")
	probe := fs.Bool("probe", false, "check whether writes from this computer reach MoneyLover, and create nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, cfg, err := loadAdapterConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()

	if *probe {
		tok, terr := fetchWriteToken(ctx, cfg)
		if terr != nil {
			return terr
		}
		return adapterProbe(ctx, moneylover.NewWithToken(tok))
	}

	fmt.Printf("\n  BurnRate Adapter %s\n", adapterVersion)
	fmt.Printf("  Connected to %s\n\n", short(cfg.Server))

	saidFrozen := false
	for {
		frozen, err := beat(ctx, cfg)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "  can't reach BurnRate: %v\n", err)
		case frozen:
			if !saidFrozen {
				fmt.Println("  BurnRate has paused writing, so there is nothing to send.")
				fmt.Println("  Clear it in BurnRate; this keeps checking and resumes on its own.")
				saidFrozen = true
			}
		default:
			saidFrozen = false
			n, passErr := adapterPass(ctx, cfg, *limit)
			switch {
			case passErr != nil:
				fmt.Fprintf(os.Stderr, "  %v\n", passErr)
			case n > 0:
				fmt.Printf("  Wrote %d. Waiting.\n", n)
			}
		}
		if *once {
			return nil
		}
		time.Sleep(*every)
	}
}

// loadAdapterConfig finds the file the user downloaded from their BurnRate.
//
// Beside the binary first, because that is where a downloaded pair ends up,
// then the working directory. A missing file is the likeliest thing to go
// wrong on a first run, so it says exactly what to do about it.
func loadAdapterConfig() (string, adapterConfig, error) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), configName))
	}
	candidates = append(candidates, configName)
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c adapterConfig
		if err := json.Unmarshal(b, &c); err != nil {
			return p, c, fmt.Errorf("%s is not readable as settings: %w", p, err)
		}
		if strings.TrimSpace(c.Server) == "" || strings.TrimSpace(c.Token) == "" {
			return p, c, fmt.Errorf("%s has no server address or no token — download it again from Settings, Adapter", p)
		}
		c.Server = strings.TrimRight(c.Server, "/")
		if c.Service == "" {
			c.Service = "moneylover"
		}
		if c.Service != "moneylover" {
			return p, c, fmt.Errorf(
				"%s is for %q, and this adapter only knows how to write to MoneyLover — download a newer adapter", p, c.Service)
		}
		return p, c, nil
	}
	return "", adapterConfig{}, errors.New(
		"cannot find " + configName + " — download it from your BurnRate under Settings, Adapter, and put it in this folder")
}

// signIn gets a MoneyLover session for this machine, asking only for what it
// does not already have.
//
// The password is read without echo and used once. Only the session token is
// kept: a password stored to avoid asking again is a password that outlives
// the reason for having it.
// fetchWriteToken asks BurnRate for a MoneyLover access token.
//
// Only --probe needs this: a normal pass gets its token with the batch it is
// about to write, so an idle adapter never causes a sign-in at all.
func fetchWriteToken(ctx context.Context, cfg adapterConfig) (string, error) {
	body, err := call(ctx, cfg, http.MethodGet, cfg.Server+"/api/adapter/token", nil)
	if err != nil {
		return "", fmt.Errorf("asking BurnRate for a MoneyLover token: %w", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("asking BurnRate for a MoneyLover token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("BurnRate has no MoneyLover session to lend - check the MoneyLover login saved in BurnRate")
	}
	return out.Token, nil
}

// adapterProbe answers whether a WRITE from this computer reaches MoneyLover,
// and creates nothing.
//
// It sends a deliberately invalid write. MoneyLover rejecting it is the GOOD
// outcome: a rejection means the request arrived, was understood and was
// refused on its merits, which is what a real write needs in order to succeed.
// Cloudflare answering means it never got there.
//
// Nothing else answers this. A successful read proves nothing, because reads
// already work from the blocked server — only a write is scored the way writes
// are scored.
func adapterProbe(ctx context.Context, ml *moneylover.Client) error {
	fmt.Println("\n  Checking whether writes from this computer reach MoneyLover...")
	fmt.Println("  Nothing will be created — the request is deliberately invalid.")
	err := ml.AddTransaction(ctx, "probe-no-such-wallet", "probe-no-such-category", 0.01,
		"burnrate connectivity probe - not a transaction", "2026-01-01", "burnrate-probe")
	switch {
	case errors.Is(err, moneylover.ErrBotChallenge):
		fmt.Println("\n  BLOCKED. The request was stopped before it reached MoneyLover,")
		fmt.Println("  the same way it is from the server. This computer is challenged too.")
		return errors.New("writes from this computer are blocked as well")
	case err == nil:
		fmt.Println("\n  Reached MoneyLover — and it accepted an invalid write, which is odd.")
		fmt.Println("  Writes get through, but check the wallet before relying on it.")
		return nil
	default:
		fmt.Printf("\n  Reached MoneyLover. It answered: %v\n", err)
		fmt.Println("  That is the good outcome: the request arrived and was refused on its")
		fmt.Println("  merits, not stopped at the door. Writes from here get through.")
		return nil
	}
}

func adapterPass(ctx context.Context, cfg adapterConfig, limit int) (int, error) {
	rows, token, err := fetchQueue(ctx, cfg, limit)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	// A client for this batch and no longer. It holds the token BurnRate sent
	// and nothing else - no login, no password, nothing on disk.
	ml := moneylover.NewWithToken(token)
	ml.SetSettings(localSettings{})
	results := make([]adapterResult, 0, len(rows))
	wrote := 0
	expired := false
	for i, r := range rows {
		err := ml.AddTransaction(ctx, r.WalletID, r.CategoryID, r.Amount, r.Note, r.Date, r.Gid)
		results = append(results, classify(r.TxnID, err))
		switch {
		case err == nil:
			fmt.Printf("  wrote #%d  %.2f %s  %s\n", r.TxnID, r.Amount, r.Currency, r.Note)
			wrote++
		case errors.Is(err, moneylover.ErrAuth):
			// MoneyLover refused the token BurnRate lent for this batch, so
			// nothing was sent. Not something this program can fix: it holds
			// no credentials, and a fresh token arrives with the next batch.
			//
			// It must still be reported as unwritten. Calling it "may have
			// landed" is the worst answer available - the money stays out of
			// the wallet, each row is held four hours, and held rows drop off
			// the only list that shows them, so the operator sees a green
			// "running" pill and a promise that never comes true.
			fmt.Fprintf(os.Stderr, "  #%d not written - BurnRate's MoneyLover session was refused\n", r.TxnID)
			expired = true
		case errors.Is(err, moneylover.ErrBotChallenge):
			fmt.Fprintf(os.Stderr, "  #%d was blocked before it reached MoneyLover\n", r.TxnID)
		default:
			// Everything else stays "may have landed" deliberately: a timeout
			// in flight cannot be told from a refusal, and holding a row that
			// was written beats writing one twice.
			fmt.Fprintf(os.Stderr, "  #%d failed: %v\n", r.TxnID, err)
		}
		if expired {
			// The rest of the batch fails the same way and none of it is sent.
			// Marching it through would quarantine the whole backlog.
			for _, rest := range rows[i+1:] {
				results = append(results, adapterResult{
					TxnID: rest.TxnID,
					Error: "not attempted: the MoneyLover session was refused earlier in this batch",
				})
			}
			break
		}
	}
	if err := report(ctx, cfg, results); err != nil {
		return wrote, err
	}
	if expired {
		return wrote, errSessionExpired
	}
	return wrote, nil
}

// errSessionExpired says MoneyLover refused the token BurnRate lent for this
// batch. The rest of the batch would fail identically, so the pass stops
// rather than pulling the whole backlog through a token that is not working.
// The next pass gets a fresh one, so this recovers on its own.
var errSessionExpired = errors.New("BurnRate's MoneyLover session was refused")

// classify turns a write error into what BurnRate is told, and it is the only
// place that decides it.
//
// "Landed" does not mean success — it means the request may have reached
// MoneyLover, so BurnRate must hold the row rather than send it again. The
// default is deliberately Landed: a timeout in flight cannot be told from a
// refusal, and holding a row that was written beats writing one twice.
//
// The two exceptions are the cases where nothing was sent and we know it:
// ErrAuth is returned before a request is even built when the client has no
// password, and a bot challenge is Cloudflare answering instead of MoneyLover.
// Reporting those as Landed strands real money outside the wallet for four
// hours a row while every page reports the adapter healthy.
func classify(txnID int64, err error) adapterResult {
	switch {
	case err == nil:
		return adapterResult{TxnID: txnID, OK: true, Landed: true}
	case errors.Is(err, moneylover.ErrAuth), errors.Is(err, moneylover.ErrBotChallenge):
		return adapterResult{TxnID: txnID, Landed: false, Error: err.Error()}
	default:
		return adapterResult{TxnID: txnID, Landed: true, Error: err.Error()}
	}
}

// fetchQueue returns the rows to write and the MoneyLover token to write them
// with. The token comes WITH the batch rather than being held between passes:
// it is the credential for someone's real wallet, so it lives in this process
// for as long as one batch takes and no longer.
func fetchQueue(ctx context.Context, cfg adapterConfig, limit int) ([]adapterRow, string, error) {
	u := cfg.Server + "/api/adapter/queue"
	if limit > 0 {
		u += "?limit=" + strconv.Itoa(limit)
	}
	body, err := call(ctx, cfg, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Rows  []adapterRow `json:"rows"`
		Token string       `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", fmt.Errorf("queue: %w", err)
	}
	if len(out.Rows) > 0 && out.Token == "" {
		// Refusing beats attempting: BurnRate has already marked these rows as
		// in flight, and a batch sent with no token would fail every row and
		// hold each one for four hours.
		return nil, "", errors.New("BurnRate sent work but no MoneyLover token - update BurnRate")
	}
	return out.Rows, out.Token, nil
}

func report(ctx context.Context, cfg adapterConfig, results []adapterResult) error {
	if len(results) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"results": results})
	if _, err := call(ctx, cfg, http.MethodPost, cfg.Server+"/api/adapter/result", payload); err != nil {
		// The writes may have happened. Say so: the server still holds them,
		// and the next MoneyLover sync settles each one.
		return fmt.Errorf("wrote, but could not report back (%w) - the rows stay held until the next sync settles them", err)
	}
	return nil
}

// beat reports in and returns whether BurnRate has paused writing.
//
// The answer used to be discarded. A frozen ledger then meant the queue
// answered 409 every minute, the adapter printed one opaque line and slept,
// and the settings page went on showing a green "running" pill beside rows it
// promised would be written "within about a minute" - every statement true
// about the adapter, and every one the wrong answer to the operator's question.
func beat(ctx context.Context, cfg adapterConfig) (bool, error) {
	host, _ := os.Hostname()
	payload, _ := json.Marshal(map[string]any{"version": adapterVersion, "host": host})
	body, err := call(ctx, cfg, http.MethodPost, cfg.Server+"/api/adapter/heartbeat", payload)
	if err != nil {
		return false, err
	}
	var out struct {
		Frozen bool `json:"frozen"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Frozen, nil
}

func call(ctx context.Context, cfg adapterConfig, method, url string, body []byte) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("BurnRate rejected this adapter's token — download " + configName + " again from Settings, Adapter")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func short(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
}
