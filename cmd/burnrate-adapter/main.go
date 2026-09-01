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
	"bufio"
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
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/azfoundary/burnrate-adapter/moneylover"
)

// adapterVersion is reported on every heartbeat so the server can say when a
// newer one is available. Bumped by hand: a version tracking the server build
// would claim a compatibility nobody tested.
const adapterVersion = "1.0.0"

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
	// Email is remembered after the first run so only the password is asked
	// for when a MoneyLover session expires. The password is never written
	// here, or anywhere.
	Email string `json:"moneylover_email,omitempty"`
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

	cfgPath, cfg, err := loadAdapterConfig()
	if err != nil && !*probe {
		return err
	}
	ctx := context.Background()

	ml, err := signIn(ctx, cfgPath, cfg)
	if err != nil {
		return err
	}
	if *probe {
		return adapterProbe(ctx, ml)
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
			n, passErr := adapterPass(ctx, ml, cfg, *limit)
			switch {
			case errors.Is(passErr, errSessionExpired):
				fmt.Fprintln(os.Stderr, "\n  Your MoneyLover session has expired. Nothing was written,")
				fmt.Fprintln(os.Stderr, "  and no transaction has been changed.")
				if !term.IsTerminal(int(syscall.Stdin)) {
					// Started at login, with no window to type into. Exiting is
					// the honest move: staying up keeps the heartbeat green
					// while nothing can ever be written.
					fmt.Fprintln(os.Stderr, "  Start the adapter from a window so it can ask you to sign in again.")
					return errSessionExpired
				}
				newML, signInErr := signIn(ctx, cfgPath, cfg)
				if signInErr != nil {
					return signInErr
				}
				ml = newML
				continue
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
func signIn(ctx context.Context, cfgPath string, cfg adapterConfig) (*moneylover.Client, error) {
	home, _ := os.UserHomeDir()
	sessionPath := filepath.Join(home, ".burnrate-adapter-session.json")

	email := cfg.Email
	if v := os.Getenv("ML_EMAIL"); v != "" {
		email = v
	}
	pass := os.Getenv("ML_PASSWORD")

	// A cached session means neither is needed. Try it before asking.
	if pass == "" && email != "" {
		if _, err := os.Stat(sessionPath); err == nil {
			ml := moneylover.New(email, "", sessionPath)
			ml.SetSettings(localSettings{})
			if _, err := ml.ListWallets(ctx); err == nil {
				return ml, nil
			}
			fmt.Println("  Your MoneyLover session has expired.")
		}
	}
	if email == "" {
		email = prompt("  MoneyLover email: ")
	}
	if pass == "" {
		pass = promptSecret("  MoneyLover password: ")
	}
	ml := moneylover.New(email, pass, sessionPath)
	ml.SetSettings(localSettings{})
	if _, err := ml.ListWallets(ctx); err != nil {
		return nil, fmt.Errorf("signing in to MoneyLover: %w", err)
	}
	fmt.Println("  Signed in. You will not be asked again until this expires.")
	// Remember the email so only the password is needed next time.
	if cfgPath != "" && cfg.Email != email {
		cfg.Email = email
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = os.WriteFile(cfgPath, b, 0o600)
		}
	}
	return ml, nil
}

func prompt(label string) string {
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
		return prompt("")
	}
	return strings.TrimSpace(string(b))
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

func adapterPass(ctx context.Context, ml *moneylover.Client, cfg adapterConfig, limit int) (int, error) {
	rows, err := fetchQueue(ctx, cfg, limit)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
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
			// Nothing was sent. loginLocked refuses before it builds a request
			// when there is no password, which is EVERY run resumed from a
			// cached session: the client is constructed with "" and there is
			// no refresh grant, so an hour in, this is the state.
			//
			// Calling that "may have landed" was the worst answer available.
			// Real money stays out of the wallet, each row is held four hours,
			// and held rows drop off the only list that shows them — so the
			// operator sees a green "running" pill and a promise that the rows
			// write within a minute, indefinitely.
			fmt.Fprintf(os.Stderr, "  #%d not written - the MoneyLover session has expired\n", r.TxnID)
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
					Error: "not attempted: the MoneyLover session expired earlier in this batch",
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

// errSessionExpired says the cached MoneyLover session has lapsed and this
// process holds no password to renew it with. Signing in again is the only
// way forward, so the loop stops asking for work rather than pulling the
// backlog through a client that cannot send it.
var errSessionExpired = errors.New("the MoneyLover session has expired")

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

func fetchQueue(ctx context.Context, cfg adapterConfig, limit int) ([]adapterRow, error) {
	u := cfg.Server + "/api/adapter/queue"
	if limit > 0 {
		u += "?limit=" + strconv.Itoa(limit)
	}
	body, err := call(ctx, cfg, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Rows []adapterRow `json:"rows"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	return out.Rows, nil
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
