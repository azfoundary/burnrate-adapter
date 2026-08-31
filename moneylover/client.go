// Package moneylover is a thin client for MoneyLover's unofficial REST API,
// a poller that ingests wallet transactions into the recon engine, and a
// balance canary that freezes ledger→MoneyLover writes on drift.
//
// There is no official API. The community-proven flow (spec §4) is:
//
//  1. POST https://web.moneylover.me/api/user/login-url
//     → {data: {login_url, request_token}}
//  2. POST https://oauth.moneylover.me/token with headers
//     "Authorization: Bearer <request_token>" and "client: <id parsed from
//     login_url query params>", JSON body {email, password}
//     → {access_token, refresh_token, expire}
//  3. Authenticated calls against https://web.moneylover.me/api with
//     "Authorization: AuthJWT <access_token>" — the only scheme this client
//     will ever send on the API (see apiPostLocked for why).
//
// Field names in responses vary across community docs, so all decoding is
// tolerant (see json.go and VERIFY.md).
package moneylover

// A client for MoneyLover's private API — the one their own web and mobile
// apps use. MoneyLover publishes no public API; this was built by reading
// their web bundle, and it only ever acts on the account whose credentials it
// is given.
//
// DUPLICATION, STATED PLAINLY: the BurnRate service (azfoundary/burnrate,
// internal/moneylover) carries its own copy of the sign-in flow and the
// sync-push envelope, because it also reads transactions and that needs the
// ledger's own timezone and row shape — neither of which belongs in a protocol
// client. So if MoneyLover changes how sign-in or the transaction push works,
// TWO places need the fix. That is a real cost, accepted because the
// alternative was moving date handling out of the ledger that owns it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase  = "https://web.moneylover.me/api"
	defaultOAuthURL = "https://oauth.moneylover.me/token"

	// Generous ceiling: the unofficial API's WRITE endpoints can stall for
	// tens of seconds while reads return instantly; callers bound their own
	// contexts tighter where it matters.
	httpTimeout = 90 * time.Second
	backoffMin  = 1 * time.Second
	backoffMax  = 5 * time.Minute

	schemeAuthJWT = "AuthJWT"
)

// Sentinel errors.
var (
	// ErrAuth marks authentication failures (bad credentials, token rejected
	// after re-login). The poller reacts by alerting + freezing writes.
	ErrAuth = errors.New("moneylover: authentication failed")
	// ErrWritesFrozen is returned by AddTransaction while the canary freeze
	// flag (store.SettingWritesFrozen) is set.
	ErrWritesFrozen = errors.New("moneylover: writes are frozen (canary drift or auth failure — clear the writes_frozen setting after investigating)")
	// ErrBackoff is returned while the client is inside its failure backoff
	// window; the next poll tick will retry.
	ErrBackoff = errors.New("moneylover: backing off after repeated failures")
	// ErrBotChallenge means the request never reached MoneyLover: the sync
	// host sits behind bot protection and answered with a challenge page.
	//
	// Worth its own error because of what it is NOT. It is not a rejected
	// payload, not bad credentials and not a wrong endpoint, and the raw body
	// is a page of HTML that reads like all three. Every WRITE goes through
	// this host — mirroring a row into the wallet and deleting one out of it
	// — while every read uses a different one, so the ledger can be syncing
	// perfectly while nothing it writes ever arrives.
	ErrBotChallenge = errors.New("moneylover: blocked by the sync host's bot protection — the request never reached MoneyLover, so nothing was written; adding and deleting wallet rows from here is unavailable until that changes (reads are unaffected)")
)

// SettingWritesFrozen is the setting key the host application uses to freeze
// writes after its own consistency check fails. Declared here rather than
// imported so this package depends on nothing outside the standard library:
// the caller supplies a SettingGetter that knows how to read it.
const SettingWritesFrozen = "writes_frozen"

// SettingGetter is the slice of the host's settings store the write path needs
// for the freeze check. Declared here so tests and other callers can stub it.
type SettingGetter interface {
	GetSetting(key string) (string, error)
}

// Wallet is one MoneyLover wallet from POST /wallet/list.
type Wallet struct {
	ID       string
	Name     string
	Currency string             // ISO code derived from the balance map (e.g. "PKR")
	Balance  map[string]float64 // currency code → amount
	Archived bool
	// AccountType is the payload's account_type — MoneyLover's per-wallet
	// write gate (see accountTypeAddRight). accountTypeUnknown when the
	// payload omits the field, deliberately NOT 0: 0 is a real, writable
	// type and must not be inferred from an absent field.
	AccountType int
	// CreatedAt is the payload's createdAt verbatim (ISO-8601 as sent).
	// Diagnostic context only — never parsed or compared.
	CreatedAt string
}

// MoneyLover gates transaction writes per wallet. Its config endpoint
// (POST /api/other/config, no auth required) publishes
// config.wallet.typeSupport = [0 2 4 5] and config.walletPolicy, in which
// Transaction.AddRight is true only for account_type 0, 4 and 5 on a
// NON-archived wallet; type 2 (linked bank wallet) is read-only even when
// active, and any type outside typeSupport falls back to walletPolicy["-1"],
// which grants nothing.
//
// This table feeds the diagnostic log ONLY — the write path deliberately does
// not gate on it, so a policy snapshot that goes stale can never block a
// write that would otherwise have succeeded.
const accountTypeUnknown = -1

var accountTypeAddRight = map[int]bool{0: true, 4: true, 5: true}

// Category is one row from POST /category/list. Type: 1=income, 2=expense.
type Category struct {
	ID   string
	Name string
	Type int
}

// tokenCache is the on-disk JWT cache (written 0600). Email binds the
// token to the account it belongs to: switching ML credentials in Settings
// must NOT keep using the previous account's still-valid token.
//
// Files written by older builds also carry "refresh_token" and "auth_scheme".
// Neither was ever read back — there is no refresh grant (loginLocked always
// runs the full two-step flow) and AuthJWT is the only scheme — so both keys
// were dropped. encoding/json ignores them, and the next save rewrites the
// file without them.
type tokenCache struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   string `json:"expires_at,omitempty"` // RFC3339
	Email       string `json:"email,omitempty"`
}

// Client is a minimal MoneyLover API client with disk-cached JWT, automatic
// re-login on 401/expiry and exponential backoff (1s..5m) on failures.
type Client struct {
	email          string
	password       string
	tokenCachePath string

	apiBase  string // overridable in tests
	oauthURL string
	// revoBase is the sync service, which is a DIFFERENT host from apiBase and
	// was hardcoded at the call site. Overridable for the same reason apiBase
	// is: a test exercising the write path could otherwise only reach the real
	// MoneyLover servers, so the one thing it must never do — send a write at
	// somebody's actual wallet — was the only thing it could do.
	revoBase string
	httpc    *http.Client

	mu          sync.Mutex // guards everything below (serializes API calls)
	settings    SettingGetter
	accessToken string
	expiresAt   time.Time // zero = unknown (rely on 401 → re-login)
	fails       int
	nextTry     time.Time
}

// New builds a client. tokenCachePath is where the JWT is persisted (0600);
// "" disables caching. No network I/O happens until the first call.
func New(email, password, tokenCachePath string) *Client {
	c := &Client{
		email:          email,
		password:       password,
		tokenCachePath: tokenCachePath,
		apiBase:        defaultAPIBase,
		oauthURL:       defaultOAuthURL,
		httpc:          &http.Client{Timeout: httpTimeout},
	}
	c.loadTokenLocked() // constructor: no concurrent access yet
	return c
}

// SetSettings wires the store used for the writes-frozen check. main MUST
// call this before AddTransaction is used; unwired clients refuse writes.
func (c *Client) SetSettings(sg SettingGetter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = sg
}

// ListWallets returns every wallet visible to the account.
func (c *Client) ListWallets(ctx context.Context) ([]Wallet, error) {
	data, err := c.do(ctx, "/wallet/list", nil)
	if err != nil {
		return nil, err
	}
	wallets, err := decodeWallets(data)
	if err != nil {
		return nil, fmt.Errorf("moneylover: wallet/list: %w", err)
	}
	return wallets, nil
}

// walletDiagLogged fingerprints the wallets logWalletDiag has already
// reported ("id|type|archived"), so a 45-minute poll loop and every writeback
// don't repeat one line forever — while a wallet whose gate actually changes
// (archived, retyped) is reported again.
var walletDiagLogged sync.Map

// logWalletDiag reports the per-wallet write gate for the configured target
// wallet: account_type, archived and createdAt, plus what MoneyLover's
// published walletPolicy says about that combination. Purely observational —
// it answers "can this wallet accept a transaction at all?" when a write
// fails, and changes nothing about what we send.
func logWalletDiag(where string, w *Wallet) {
	if w == nil {
		return
	}
	key := fmt.Sprintf("%s|%d|%t", w.ID, w.AccountType, w.Archived)
	if _, seen := walletDiagLogged.LoadOrStore(key, struct{}{}); seen {
		return
	}
	verdict := "writable — walletPolicy grants Transaction.AddRight"
	switch {
	case w.AccountType == accountTypeUnknown:
		verdict = "UNKNOWN — /wallet/list carried no account_type field"
	case !accountTypeAddRight[w.AccountType]:
		verdict = "NOT writable — walletPolicy denies Transaction.AddRight for this account_type"
	case w.Archived:
		verdict = "NOT writable — walletPolicy denies Transaction.AddRight on archived wallets"
	}
	log.Printf("moneylover: VERIFY wallet %q (%s, via %s) — account_type=%d archived=%t created_at=%q → %s",
		w.Name, w.ID, where, w.AccountType, w.Archived, w.CreatedAt, verdict)
}

// ListCategories pulls the wallet's categories (needed to pick a categoryID
// for AddTransaction).
func (c *Client) ListCategories(ctx context.Context, walletID string) ([]Category, error) {
	data, err := c.do(ctx, "/category/list", map[string]string{"walletId": walletID})
	if err != nil {
		return nil, err
	}
	cats, err := decodeCategories(data)
	if err != nil {
		return nil, fmt.Errorf("moneylover: category/list: %w", err)
	}
	return cats, nil
}

// revoHost is the MoneyLover SYNC service. Transaction creation lives here
// (the same endpoint MoneyLover's own web/mobile apps use), NOT on the
// /transaction/add route of the main API — that route hangs for third-party
// clients. Reads stay on the main API under AuthJWT (unchanged).
const revoHost = "https://revoapi.moneylover.me"

// revoClientID is the PUBLIC client identifier shipped in MoneyLover's web
// app bundle (visible in every browser). Required as the "client" header on
// every revo call, or the server answers "Oauth expire" for valid tokens.
const revoClientID = "kHiZbFQOw5LV"

// AddTransaction creates a transaction in MoneyLover via the sync service,
// authenticated with THIS account's own token. gid is a client-generated
// stable id (idempotency key): re-sending the same gid upserts rather than
// duplicating, so a retry is safe. Gated on the canary freeze flag.
func (c *Client) AddTransaction(ctx context.Context, walletID, categoryID string, amount float64, note, date, gid string) error {
	c.mu.Lock()
	sg := c.settings
	c.mu.Unlock()
	if sg == nil {
		return fmt.Errorf("moneylover: no settings store wired (Client.SetSettings) — refusing write, cannot verify freeze flag")
	}
	frozen, err := sg.GetSetting(SettingWritesFrozen)
	if err != nil {
		return fmt.Errorf("moneylover: freeze check failed, refusing write: %w", err)
	}
	if frozen == "1" {
		return ErrWritesFrozen
	}

	// Inner sync doc: one transaction, compact two-letter keys. The outer
	// envelope carries it as a STRING (double-encoded), per the sync codec.
	inner := map[string]any{"d": []any{map[string]any{
		"gid": gid, "ac": walletID, "c": categoryID, "a": amount,
		"n": note, "dd": date, "er": false, "mr": false,
		"lo": 0, "la": 0, "ad": "", "oc": "", "pi": "", "v": 0, "rd": 0, "f": 1,
		"im": []any{},
	}}}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return fmt.Errorf("moneylover: marshal sync doc: %w", err)
	}
	envelope, err := json.Marshal(map[string]any{"pl": 8, "av": 696, "data": string(innerJSON)})
	if err != nil {
		return fmt.Errorf("moneylover: marshal envelope: %w", err)
	}

	resp, err := c.revoPost(ctx, "/api/sync/push/transaction/v2", envelope)
	if err != nil {
		return err
	}
	// The sync API returns 2xx with {status:false} or a non-empty
	// failedItems on rejection — HTTP 200 alone is not success.
	var r struct {
		Status      *bool             `json:"status"`
		Error       int               `json:"error"`
		Message     string            `json:"message"`
		Data        json.RawMessage   `json:"data"`
		FailedItems []json.RawMessage `json:"failedItems"`
	}
	_ = json.Unmarshal(resp, &r)
	if r.Error != 0 || (r.Status != nil && !*r.Status) || len(r.FailedItems) > 0 {
		return fmt.Errorf("moneylover: sync push rejected: error=%d msg=%q failed=%d body=%s",
			r.Error, r.Message, len(r.FailedItems), snippet(resp))
	}
	return nil
}

// botChallenge reports whether a response is an anti-bot interstitial rather
// than anything MoneyLover said.
//
// Cloudflare answers 403 or 503 with an HTML page titled "Just a moment…";
// reporting that verbatim sends the next reader hunting for a payload bug in a
// request the API never saw.
func botChallenge(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable {
		return false
	}
	s := strings.ToLower(string(body))
	for _, m := range []string{"just a moment", "cf-browser-verification", "__cf_chl", "attention required", "cf-please-wait"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// revoPost issues one authenticated POST to the sync service. The account's
// own access token is sent as Bearer here (the main API uses AuthJWT); the
// public client-id + version headers are mandatory.
func (c *Client) revoPost(ctx context.Context, path string, payload []byte) ([]byte, error) {
	c.mu.Lock()
	if err := c.ensureTokenLocked(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	token := c.accessToken
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	base := c.revoBase
	if base == "" {
		base = revoHost
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("client", revoClientID)
	req.Header.Set("apiversion", "4")
	req.Header.Set("appversion", "696")
	req.Header.Set("dataformat", "json")
	req.Header.Set("platform", "8")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("moneylover: revo %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// The edge's own account of what it did, logged before we guess from
		// the body. botChallenge reads status and HTML text, which cannot
		// separate "Cloudflare stopped this" from "the origin answered 403
		// with an error page" — and those two have nothing in common as
		// problems. cf-mitigated says which; cf-ray identifies the request if
		// anyone ever asks the vendor about it.
		//
		// It matters here more than it usually would: this endpoint accepted
		// a live write on 2026-08-11 and refused one on 2026-08-30, so
		// whatever changed is worth naming exactly rather than inferring.
		log.Printf("moneylover: revo %s refused: HTTP %d cf-mitigated=%q cf-ray=%q server=%q body=%s",
			path, resp.StatusCode,
			resp.Header.Get("cf-mitigated"), resp.Header.Get("cf-ray"),
			resp.Header.Get("server"), snippet(b))
	}
	if botChallenge(resp.StatusCode, b) {
		return nil, fmt.Errorf("%w (HTTP %d from %s)", ErrBotChallenge, resp.StatusCode, path)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("moneylover: revo %s: HTTP %d: %s", path, resp.StatusCode, snippet(b))
	}
	return b, nil
}

// --- transport / auth core -------------------------------------------------

// do is the single entry point for authenticated API calls: it serializes
// access, applies the failure backoff gate, and unwraps the envelope.
func (c *Client) do(ctx context.Context, path string, body any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.nextTry.IsZero() && time.Now().Before(c.nextTry) {
		return nil, fmt.Errorf("%w (retry after %s, %d consecutive failures)",
			ErrBackoff, c.nextTry.Format(time.RFC3339), c.fails)
	}
	data, err := c.apiPostLocked(ctx, path, body, true)
	if err != nil {
		c.fails++
		d := backoffMin
		for i := 1; i < c.fails && d < backoffMax; i++ {
			d *= 2
		}
		if d > backoffMax {
			d = backoffMax
		}
		c.nextTry = time.Now().Add(d)
		return nil, err
	}
	c.fails = 0
	c.nextTry = time.Time{}
	return data, nil
}

// apiPostLocked performs one authenticated POST under AuthJWT, with (once)
// a full re-login on 401. mu must be held.
//
// AuthJWT is the ONLY scheme: two weeks of production evidence showed the
// Bearer scheme half-authenticates on this deployment — requests succeed
// with well-formed but EMPTY data (wallet lists, etc.), which poisoned the
// cached scheme preference twice and silently broke every read for days.
// A denied/hanging call must surface as an error, never fall through to a
// scheme that fabricates empty success.
func (c *Client) apiPostLocked(ctx context.Context, path string, body any, allowRelogin bool) (json.RawMessage, error) {
	if err := c.ensureTokenLocked(ctx); err != nil {
		return nil, err
	}
	payload := []byte("{}")
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("moneylover: marshal body: %w", err)
		}
	}
	status, respBody, err := c.httpPost(ctx, c.apiBase+path, payload,
		map[string]string{"Authorization": schemeAuthJWT + " " + c.accessToken})
	if err != nil {
		return nil, fmt.Errorf("moneylover: %s: %w", path, err)
	}
	if status == http.StatusUnauthorized {
		if allowRelogin {
			c.accessToken = ""
			if err := c.loginLocked(ctx); err != nil {
				return nil, err
			}
			return c.apiPostLocked(ctx, path, body, false)
		}
		return nil, fmt.Errorf("%w: %s rejected the token", ErrAuth, path)
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("moneylover: %s: HTTP %d: %s", path, status, snippet(respBody))
	}
	data, err := parseEnvelope(respBody)
	if err != nil {
		return nil, fmt.Errorf("moneylover: %s: %w", path, err)
	}
	return data, nil
}

// ensureTokenLocked logs in when there is no token or the cached one is
// (about to be) expired. mu must be held.
func (c *Client) ensureTokenLocked(ctx context.Context) error {
	if c.accessToken != "" && (c.expiresAt.IsZero() || time.Now().Before(c.expiresAt.Add(-time.Minute))) {
		return nil
	}
	return c.loginLocked(ctx)
}

// loginLocked runs the two-step login flow. mu must be held.
func (c *Client) loginLocked(ctx context.Context) error {
	if c.email == "" || c.password == "" {
		return fmt.Errorf("%w: ML_EMAIL / ML_PASSWORD not configured", ErrAuth)
	}

	// Step 1: obtain request_token + client id.
	status, body, err := c.httpPost(ctx, c.apiBase+"/user/login-url", []byte("{}"), nil)
	if err != nil {
		return fmt.Errorf("moneylover: login-url: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("moneylover: login-url HTTP %d (transient/blocked, not treated as auth failure): %s", status, snippet(body))
	}
	data, err := parseEnvelope(body)
	if err != nil {
		return fmt.Errorf("moneylover: login-url: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("moneylover: login-url payload: %s", snippet(data))
	}
	loginURL := rawStr(m, "login_url", "loginUrl", "loginURL")
	reqToken := rawStr(m, "request_token", "requestToken", "token")
	if loginURL == "" || reqToken == "" {
		return fmt.Errorf("moneylover: login-url response missing login_url/request_token (API shape drift?): %s", snippet(data))
	}
	clientID := clientIDFromLoginURL(loginURL)
	if clientID == "" {
		return fmt.Errorf("moneylover: no client id in login_url %q (API shape drift?)", loginURL)
	}

	// Step 2: exchange credentials for tokens.
	payload, _ := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	status, body, err = c.httpPost(ctx, c.oauthURL, payload, map[string]string{
		"Authorization": "Bearer " + reqToken,
		"client":        clientID,
	})
	if err != nil {
		return fmt.Errorf("moneylover: oauth token: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w: oauth token HTTP %d: %s", ErrAuth, status, snippet(body))
	}
	if status/100 != 2 {
		return fmt.Errorf("moneylover: oauth token HTTP %d: %s", status, snippet(body))
	}
	var tm map[string]json.RawMessage
	if err := json.Unmarshal(body, &tm); err != nil {
		return fmt.Errorf("moneylover: oauth token payload: %s", snippet(body))
	}
	if inner := rawObj(tm, "data"); inner != nil { // tolerate an envelope here too
		tm = inner
	}
	access := rawStr(tm, "access_token", "accessToken")
	if access == "" {
		return fmt.Errorf("moneylover: no access_token in oauth response (API shape drift?): %s", snippet(body))
	}
	c.accessToken = access
	c.expiresAt = parseExpire(tm["expire"])
	c.saveTokenLocked()
	log.Printf("moneylover: logged in, token cached (expires %s)", c.expiresAtString())
	return nil
}

func (c *Client) expiresAtString() string {
	if c.expiresAt.IsZero() {
		return "unknown"
	}
	return c.expiresAt.Format(time.RFC3339)
}

// clientIDFromLoginURL extracts the OAuth client id from the login_url query
// string (community docs show it as the "client" parameter).
func clientIDFromLoginURL(loginURL string) string {
	u, err := url.Parse(loginURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	for _, key := range []string{"client", "client_id", "clientId"} {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// parseExpire tolerates every "expire" shape seen in community docs:
// RFC3339 string, epoch seconds, epoch milliseconds, or a relative
// seconds-from-now duration. Unknown shapes → zero time (rely on 401).
func parseExpire(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t
			}
		}
	}
	if f, ok := parseNum(raw); ok && f > 0 {
		switch {
		case f > 1e12: // epoch milliseconds
			return time.UnixMilli(int64(f))
		case f > 1e9: // epoch seconds
			return time.Unix(int64(f), 0)
		default: // seconds from now
			return time.Now().Add(time.Duration(f) * time.Second)
		}
	}
	return time.Time{}
}

// httpPost issues one POST with JSON body, the package timeout and the given
// extra headers, returning status + body. It never interprets the payload.
func (c *Client) httpPost(ctx context.Context, u string, payload []byte, headers map[string]string) (int, []byte, error) {
	tctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, u, strings.NewReader(string(payload)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache, max-age=0, no-store, no-transform, must-revalidate")
	// This app identifies itself. A spoofed Chrome User-Agent, Origin and
	// Referer used to sit here, added in July to get a write past the edge
	// protection that was stalling it. It did not work — the write route was
	// abandoned two weeks later — and by 30 August the sync host was
	// answering with cf-mitigated:challenge, which is the vendor stating
	// plainly that it does not want non-browser clients writing.
	//
	// Pretending harder is not the answer to that, and it stopped being
	// necessary: writes now go through the adapter, from a computer whose
	// requests MoneyLover accepts on their own merits.
	req.Header.Set("User-Agent", "BurnRate/1.0 (+https://github.com/azfoundary/burnrate)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// --- token cache ------------------------------------------------------------

func (c *Client) loadTokenLocked() {
	if c.tokenCachePath == "" {
		return
	}
	b, err := os.ReadFile(c.tokenCachePath)
	if err != nil {
		return // no cache yet
	}
	var tc tokenCache
	if err := json.Unmarshal(b, &tc); err != nil || tc.AccessToken == "" {
		return
	}
	if tc.Email != "" && !strings.EqualFold(tc.Email, c.email) {
		log.Printf("moneylover: token cache belongs to a different account — ignoring (fresh login)")
		return
	}
	c.accessToken = tc.AccessToken
	if tc.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, tc.ExpiresAt); err == nil {
			c.expiresAt = t
		}
	}
}

func (c *Client) saveTokenLocked() {
	if c.tokenCachePath == "" {
		return
	}
	tc := tokenCache{
		AccessToken: c.accessToken,
		Email:       c.email,
	}
	if !c.expiresAt.IsZero() {
		tc.ExpiresAt = c.expiresAt.Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.tokenCachePath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if err := os.WriteFile(c.tokenCachePath, b, 0o600); err != nil {
		log.Printf("moneylover: could not persist token cache: %v", err)
	}
}

// --- wallet / category decoding ---------------------------------------------

// decodeWallets parses the /wallet/list payload. Balances arrive as an array
// of single-key maps: [{"PKR": "123,456.00"}].
func decodeWallets(data []byte) ([]Wallet, error) {
	items, err := rawList(data, "wallets")
	if err != nil {
		return nil, err
	}
	wallets := make([]Wallet, 0, len(items))
	for _, item := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(item, &m) != nil {
			continue // tolerate junk entries
		}
		w := Wallet{
			ID:          rawStr(m, "_id", "id"),
			Name:        rawStr(m, "name"),
			Balance:     map[string]float64{},
			Archived:    rawBool(m, "archived"),
			AccountType: accountTypeUnknown,
			CreatedAt:   rawStr(m, "createdAt", "created_at"),
		}
		if f, ok := rawNum(m, "account_type", "accountType"); ok {
			w.AccountType = int(f)
		}
		for _, b := range rawArr(m, "balance") {
			var bm map[string]json.RawMessage
			if json.Unmarshal(b, &bm) != nil {
				continue
			}
			for code, v := range bm {
				if f, ok := parseNum(v); ok {
					w.Balance[strings.ToUpper(strings.TrimSpace(code))] = f
				}
			}
		}
		w.Currency = walletCurrency(w.Balance, m)
		if w.ID != "" {
			wallets = append(wallets, w)
		}
	}
	return wallets, nil
}

// walletCurrency derives the wallet currency: the balance map's key when
// unambiguous (preferring PKR, then alphabetical for determinism), otherwise
// a probe of an embedded currency object.
func walletCurrency(balance map[string]float64, m map[string]json.RawMessage) string {
	if len(balance) > 0 {
		if _, ok := balance["PKR"]; ok {
			return "PKR"
		}
		codes := make([]string, 0, len(balance))
		for code := range balance {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		return codes[0]
	}
	if cur := rawObj(m, "currency"); cur != nil {
		if code := strings.ToUpper(rawStr(cur, "code", "iso", "cur_code")); len(code) == 3 {
			return code
		}
	}
	return ""
}

func decodeCategories(data []byte) ([]Category, error) {
	items, err := rawList(data, "categories")
	if err != nil {
		return nil, err
	}
	cats := make([]Category, 0, len(items))
	for _, item := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		c := Category{ID: rawStr(m, "_id", "id"), Name: rawStr(m, "name")}
		if f, ok := rawNum(m, "type"); ok {
			c.Type = int(f)
		}
		if c.ID != "" {
			cats = append(cats, c)
		}
	}
	return cats, nil
}
