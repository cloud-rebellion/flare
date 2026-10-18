// Package automationdelivery defines the bounded, payload-free contract between
// Flare's signed automation sender and its durable delivery ledger.
package automationdelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Schema          = "flare.automation.v1"
	HTTP            = "http"
	ReactorV1       = "reactor-v1"
	MaxReceiptBytes = 2048
)

var ErrReceipt = errors.New("automation receiver acknowledgement is invalid or unavailable")

type NoticePayload struct {
	Schema     string `json:"schema"`
	Event      string `json:"event"`
	ProjectID  string `json:"project_id"`
	IssueID    string `json:"issue_id"`
	EventCount int64  `json:"event_count"`
	ObservedAt string `json:"observed_at"`
}

func NoticeBody(projectID, issueID, event string, count int64, observedAt time.Time) ([]byte, error) {
	if strings.TrimSpace(projectID) == "" || count < 0 || count > 1_000_000_000 {
		return nil, errors.New("automation notice requires a project and bounded event count")
	}
	return json.Marshal(NoticePayload{Schema, event, projectID, issueID, count, observedAt.UTC().Format(time.RFC3339Nano)})
}

func Digest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

// Acknowledgement proves HTTP delivery, a receiver enqueue, or a no-op test.
// It is not evidence of AI execution, a successful workflow, approval or a fix.
type Acknowledgement struct {
	Mode          string
	HTTPStatus    int
	RequestSHA256 string
	RunID         string
	Deduped       *bool
	Test          bool
}

func ConfiguredMode(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", ErrReceipt
	}
	value, present := fields["receipt_mode"]
	if !present {
		return HTTP, nil
	}
	var mode string
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &mode) != nil {
		return "", ErrReceipt
	}
	switch mode {
	case "", HTTP:
		return HTTP, nil
	case ReactorV1:
		return ReactorV1, nil
	default:
		return "", ErrReceipt
	}
}

func ValidRunID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r) {
			continue
		}
		return false
	}
	return true
}

func (a Acknowledgement) Valid(event, expectedDigest string) bool {
	if a.HTTPStatus < 200 || a.HTTPStatus >= 300 || a.RequestSHA256 != expectedDigest || len(expectedDigest) != 64 {
		return false
	}
	for _, r := range expectedDigest {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	if a.Mode == HTTP {
		return a.RunID == "" && a.Deduped == nil && !a.Test
	}
	if a.Mode != ReactorV1 {
		return false
	}
	if event == "test" {
		return a.Test && a.RunID == "" && a.Deduped == nil
	}
	return !a.Test && ValidRunID(a.RunID) && a.Deduped != nil
}

// ParseReactor accepts exactly the receiver's existing response projection.
// Duplicate/unknown fields, nulls, extra JSON and raw error text are refused.
func ParseReactor(raw []byte, event string, status int, requestDigest string) (*Acknowledgement, error) {
	if len(raw) == 0 || len(raw) > MaxReceiptBytes || !utf8.Valid(raw) {
		return nil, ErrReceipt
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return nil, ErrReceipt
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, ErrReceipt
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrReceipt
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrReceipt
		}
		fields[key] = value
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') || dec.Decode(&struct{}{}) != io.EOF || len(fields) != 3 {
		return nil, ErrReceipt
	}
	var accepted bool
	if json.Unmarshal(fields["accepted"], &accepted) != nil || !accepted {
		return nil, ErrReceipt
	}
	a := &Acknowledgement{Mode: ReactorV1, HTTPStatus: status, RequestSHA256: requestDigest}
	if event == "test" {
		var test, dispatched bool
		if json.Unmarshal(fields["test"], &test) != nil || !test || json.Unmarshal(fields["dispatched"], &dispatched) != nil || dispatched {
			return nil, ErrReceipt
		}
		a.Test = true
	} else {
		var deduped bool
		if json.Unmarshal(fields["deduped"], &deduped) != nil || json.Unmarshal(fields["run_id"], &a.RunID) != nil {
			return nil, ErrReceipt
		}
		a.Deduped = &deduped
	}
	if !a.Valid(event, requestDigest) {
		return nil, ErrReceipt
	}
	return a, nil
}
