package moneylover

// Defensive JSON decoding helpers. MoneyLover has no official API and the
// community docs disagree on exact field names and value types (numbers vs
// strings, wrapped vs bare payloads), so nothing here decodes strictly:
// every accessor probes a list of candidate keys and tolerates type drift.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// snippet truncates a foreign API's response body down to something that fits
// in one error line. internal/clickup, internal/moneylover and internal/vision
// each keep their own copy of this: they share no package that would be a sane
// home for six lines (internal/recon is the near-miss, and internal/vision does
// not import it), and inventing a package for a byte-truncator costs more than
// the duplication does. The copies must stay IDENTICAL, though — they had
// already drifted to 200 bytes cut silently in clickup, 200 with a marker here
// and 300 with a marker in vision. The merged behaviour is 300 plus the "…":
// the marker because otherwise an operator reading
// `HTTP 400: {"error":1,"msg":"Your session` cannot tell whether the API
// stopped there or we did, and 300 because the longest of the three envelopes
// — an OpenAI-compatible provider error, whose readable text sits nested in
// error.message behind type/param/code — is the one that gets cut before it
// says anything at 200. Change one, change all three.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// parseEnvelope unwraps the common MoneyLover response envelope
// {error: 0, msg: "...", data: ...}. A non-zero/true "error" becomes a Go
// error; a missing envelope returns the body unchanged (some endpoints
// respond with the payload directly).
func parseEnvelope(body []byte) (json.RawMessage, error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil, fmt.Errorf("empty response body")
	}
	if t[0] == '[' {
		return json.RawMessage(t), nil // bare array — no envelope
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, fmt.Errorf("unexpected response shape: %s", snippet(t))
	}
	if e, ok := m["error"]; ok && !rawFalsy(e) {
		msg := rawStr(m, "msg", "message")
		if msg == "" {
			msg = snippet(e)
		}
		return nil, fmt.Errorf("api error: %s", msg)
	}
	if d, ok := m["data"]; ok {
		return d, nil
	}
	return json.RawMessage(t), nil
}

// rawFalsy reports whether a raw JSON value is 0 / false / null / "" —
// i.e. the "no error" values seen for the envelope's error field.
func rawFalsy(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "0", "false", "null", `""`, `"0"`, `"false"`:
		return true
	}
	return false
}

// rawStr returns the first candidate key that holds a string (or a number,
// formatted as a string). Objects/arrays yield "".
func rawStr(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
			continue
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return ""
}

// rawNum returns the first candidate key that parses as a number. String
// values are tolerated ("15,000.50" → 15000.5).
//
// "First that PARSES", not "first that is present": a key whose value is null
// is skipped and the probe carries on, exactly as rawStr and rawObj already
// did. That only became true when parseNum learned to reject null — before, a
// payload carrying `"account_type": null` alongside a real `"accountType": 3`
// answered 0 and never looked at the second key.
func rawNum(m map[string]json.RawMessage, keys ...string) (float64, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		if f, ok := parseNum(raw); ok {
			return f, true
		}
	}
	return 0, false
}

// parseNum parses a raw JSON value as float64: JSON number, or a string with
// optional thousands separators / surrounding spaces.
//
// The contract is four answers: a JSON number is the value; a numeric string
// is the value; null or an absent field is ABSENT; anything else — an object,
// an array, a bool, a string that is not a number — is absent.
//
// Null costs a line of its own because Go will not give it for free.
// Unmarshalling null into a float64 SUCCEEDS and leaves the variable alone, so
// the obvious `json.Unmarshal(raw, &f)` reported (0, true) for a null. In a
// LEDGER that is the worst possible answer: a transaction whose amount the API
// declined to state became a transaction that cost nothing, and rawNum's
// multi-key probe stopped dead on the null instead of trying the next spelling
// of the key. MoneyLover has no official API and its field names are guesses,
// so probing past a null is not a corner case here — it is the normal path.
//
// internal/fx (toFloat) and internal/clickup (cuField.number) decode this same
// shape for their own APIs, and the four answers above are the contract all
// three keep — they had drifted, with clickup alone reading null correctly.
// They stay three functions on purpose. No package is a home: internal/recon
// is the only one all three import and it is the dedup/reconciliation core
// over store rows, which has never imported encoding/json and would be
// exporting a decoder it never calls. More to the point they must NOT become
// one function, and the separator stripping below is why. Grouped amounts are
// real in MoneyLover payloads, so stripping "," and " " is right here; in fx
// the same line would read a mirror's comma-decimal "1,5" as 15 and publish an
// exchange rate ten times too high. Shared contract, local tolerances — this
// is the local tolerance, and it does not travel. Change the contract in one,
// change it in all three.
func parseNum(raw json.RawMessage) (float64, bool) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, ",", ""), " ", ""))
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// rawBool returns the first candidate key that reads as true
// (bool true, non-zero number, "true"/"1").
func rawBool(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			return b
		}
		if f, ok := parseNum(raw); ok {
			return f != 0
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			s = strings.ToLower(strings.TrimSpace(s))
			return s == "true" || s == "1"
		}
	}
	return false
}

// rawObj returns the first candidate key that holds a JSON object.
func rawObj(m map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
			return obj
		}
	}
	return nil
}

// rawArr returns the first candidate key that holds a JSON array.
func rawArr(m map[string]json.RawMessage, keys ...string) []json.RawMessage {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			return arr
		}
	}
	return nil
}

// rawList extracts a JSON array from data that is either the array itself or
// an object holding it under one of the candidate keys (or "data", possibly
// nested one level: {"data": {"transactions": [...]}}).
func rawList(data []byte, keys ...string) ([]json.RawMessage, error) {
	t := bytes.TrimSpace(data)
	if len(t) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	if t[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil, fmt.Errorf("bad array payload: %s", snippet(t))
		}
		return arr, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, fmt.Errorf("unexpected payload shape: %s", snippet(t))
	}
	for _, k := range append(append([]string{}, keys...), "data") {
		raw, ok := m[k]
		if !ok {
			continue
		}
		rt := bytes.TrimSpace(raw)
		if len(rt) == 0 {
			continue
		}
		switch rt[0] {
		case '[':
			var arr []json.RawMessage
			if err := json.Unmarshal(rt, &arr); err == nil {
				return arr, nil
			}
		case '{':
			if arr, err := rawList(rt, keys...); err == nil {
				return arr, nil
			}
		}
	}
	return nil, fmt.Errorf("no list found in payload: %s", snippet(t))
}
