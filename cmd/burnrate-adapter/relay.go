package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/azfoundary/burnrate-adapter/moneylover"
)

// relayJob is one wallet read BurnRate wants made from this computer.
type relayJob struct {
	ID      string          `json:"id"`
	Path    string          `json:"path"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// reader holds the MoneyLover session used for relayed reads.
//
// Kept across passes, unlike the write path's per-batch token. The difference
// is where the credential comes from: writes borrow a session BurnRate already
// has, so there is nothing to keep, while reads exist precisely because
// BurnRate has nothing to lend. This client signs in with the login saved on
// this computer, and it holds the password for the life of the process so it
// can renew itself — which is what makes unattended running possible at all.
type reader struct {
	mu sync.Mutex
	ml *moneylover.Client
}

func (r *reader) client() (*moneylover.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ml != nil {
		return r.ml, nil
	}
	email, password, err := loadCreds()
	if err != nil {
		return nil, err
	}
	ml := moneylover.New(email, password, sessionCachePath())
	ml.SetSettings(localSettings{})
	r.ml = ml
	return ml, nil
}

// forget drops the session so the next read signs in again. Called after the
// login changes.
func (r *reader) forget() {
	r.mu.Lock()
	r.ml = nil
	r.mu.Unlock()
}

// serveReads makes the wallet reads BurnRate is waiting on, and reports how
// many it did.
//
// Every job is answered, including the ones that fail. A read BurnRate never
// hears about stalls its sync for the full relay timeout and then reports the
// adapter as absent — which is a worse diagnosis than the truth, and sends the
// operator to look at the wrong thing.
func serveReads(ctx context.Context, cfg adapterConfig, rd *reader, u ui) (int, error) {
	jobs, err := fetchJobs(ctx, cfg)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	ml, cerr := rd.client()
	for _, j := range jobs {
		if cerr != nil {
			reportJob(ctx, cfg, j.ID, nil, cerr.Error())
			continue
		}
		body, err := ml.RawRead(ctx, j.Path, j.Payload)
		if err != nil {
			u.Logf("read %s failed: %v", j.Path, err)
			reportJob(ctx, cfg, j.ID, nil, err.Error())
			continue
		}
		reportJob(ctx, cfg, j.ID, body, "")
	}
	if cerr != nil {
		return 0, cerr
	}
	return len(jobs), nil
}

func fetchJobs(ctx context.Context, cfg adapterConfig) ([]relayJob, error) {
	body, err := call(ctx, cfg, http.MethodGet, cfg.Server+"/api/adapter/jobs", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Jobs []relayJob `json:"jobs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("reading the job list: %w", err)
	}
	return out.Jobs, nil
}

func reportJob(ctx context.Context, cfg adapterConfig, id string, body []byte, errMsg string) {
	payload, _ := json.Marshal(map[string]any{"id": id, "body": body, "error": errMsg})
	_, _ = call(ctx, cfg, http.MethodPost, cfg.Server+"/api/adapter/jobs/result", payload)
}
