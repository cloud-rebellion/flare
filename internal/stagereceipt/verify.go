// Package stagereceipt checks a bounded, Stage-owned execution attestation.
// It only accepts an operator-configured HTTPS endpoint. Tenant-supplied
// receipt references never become URLs or credentials.
package stagereceipt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/bright-interaction/flare/internal/netguard"
)

const maxResponseBytes = 16 << 10

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var attestationIDPattern = regexp.MustCompile(`^stgatt_[0-9a-f]{64}$`)

type Claim struct {
	ProjectID      string `json:"project_id"`
	Kind           string `json:"kind"`
	Repository     string `json:"repository"`
	ProgramID      string `json:"program_id"`
	Pipeline       string `json:"pipeline"`
	RunRef         string `json:"run_ref"`
	EvidenceRef    string `json:"evidence_ref"`
	SourceRevision string `json:"source_revision"`
	TargetRevision string `json:"target_revision"`
}

type Attestation struct {
	Verified       bool      `json:"verified"`
	AttestationID  string    `json:"attestation_id"`
	ProjectID      string    `json:"project_id"`
	TenantID       string    `json:"tenant_id"`
	CredentialID   string    `json:"credential_id"`
	Repository     string    `json:"repository"`
	ProgramID      string    `json:"program_id"`
	Pipeline       string    `json:"pipeline"`
	Kind           string    `json:"kind"`
	RunRef         string    `json:"run_ref"`
	EvidenceRef    string    `json:"evidence_ref"`
	SourceRevision string    `json:"source_revision"`
	TargetRevision string    `json:"target_revision"`
	VerifiedAt     time.Time `json:"verified_at"`
	ImageRef       string    `json:"image_ref"`
	ImageID        string    `json:"image_id"`
}

// ErrNoProof includes both a genuine Stage negative answer and any mismatch
// between its answer and Flare's server-owned project binding/receipt fields.
var ErrNoProof = errors.New("Stage did not verify this exact receipt")
var ErrUnavailable = errors.New("Stage verifier unavailable")

type Client struct {
	http       *http.Client
	invalidPin bool
	peerIP     net.IP
}

// NewClient may pin one exact operator-approved peer IP. The request keeps its
// original HTTPS hostname, so Go still verifies Stage's certificate for that
// hostname. No proxy, redirect, DNS fallback, or broad private range exception.
func NewClient(peerIP string) *Client {
	var pin net.IP
	if peerIP != "" {
		pin = net.ParseIP(peerIP)
	}
	invalidPin := peerIP != "" && (pin == nil || pin.IsUnspecified() || pin.IsMulticast())
	dialer := &net.Dialer{
		Timeout: 4 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if pin != nil {
				if ip != nil && ip.Equal(pin) {
					return nil
				}
				return errors.New("Stage peer does not match pinned IP")
			}
			if netguard.IsBlockedIP(ip) {
				return errors.New("blocked destination address")
			}
			return nil
		},
	}
	dialContext := dialer.DialContext
	if pin != nil {
		dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			target, err := pinnedDialTarget(address, pin)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, target)
		}
	}
	return &Client{peerIP: pin, invalidPin: invalidPin, http: &http.Client{
		Timeout:       7 * time.Second,
		Transport:     &http.Transport{DialContext: dialContext, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects disabled") },
	}}
}

// ValidatePeerIP rejects a malformed or non-routable operator pin before a
// project binding is shown as usable. Empty keeps the normal DNS dial guard.
func ValidatePeerIP(raw string) error {
	if raw == "" {
		return nil
	}
	ip := net.ParseIP(raw)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return errors.New("invalid Stage peer IP")
	}
	return nil
}

func pinnedDialTarget(address string, pin net.IP) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" || pin == nil {
		return "", errors.New("invalid Stage peer port or pin")
	}
	return net.JoinHostPort(pin.String(), port), nil
}

// ValidateEndpoint requires the precise Stage attestation route, with no
// userinfo, query, fragment, alternate port, private IP or internal hostname.
// DNS results are checked again by Client's post-resolution dial guard.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.Path != "/attestations/verify" || u.RawPath != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return errors.New("Stage verifier URL must be the HTTPS /attestations/verify endpoint")
	}
	return netguard.ValidatePublicURL(raw)
}

// Verify asks Stage for a durable proof. It never trusts `verified:true` alone:
// every returned scope and immutable receipt field must match the request and
// the server-owned Stage tenant. A Stage error cannot promote an advisory row.
func (c *Client) Verify(ctx context.Context, endpoint, token, tenantID, credentialID string, claim Claim) (Attestation, error) {
	if c == nil || c.http == nil || c.invalidPin || token == "" || strings.ContainsAny(token, "\r\n") || tenantID == "" || credentialID == "" ||
		ValidateEndpoint(endpoint) != nil || claim.ProjectID == "" || claim.Repository == "" ||
		claim.ProgramID == "" || claim.Pipeline == "" || claim.RunRef == "" || claim.TargetRevision == "" {
		return Attestation{}, ErrUnavailable
	}
	if claim.Kind != "ci" && claim.Kind != "merge" && claim.Kind != "deploy" && claim.Kind != "synthetic" {
		return Attestation{}, ErrNoProof
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return Attestation{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Attestation{}, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Attestation{}, fmt.Errorf("%w: Stage returned %d", ErrUnavailable, resp.StatusCode)
	}
	limited, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(limited) > maxResponseBytes {
		return Attestation{}, ErrUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(limited))
	var proof Attestation
	if err := dec.Decode(&proof); err != nil {
		return Attestation{}, ErrUnavailable
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Attestation{}, ErrUnavailable
	}
	if !proof.Verified || !attestationIDPattern.MatchString(proof.AttestationID) ||
		proof.VerifiedAt.IsZero() || proof.VerifiedAt.After(time.Now().Add(5*time.Minute)) ||
		proof.ProjectID != claim.ProjectID || proof.TenantID != tenantID || proof.CredentialID != credentialID ||
		proof.Repository != claim.Repository || proof.ProgramID != claim.ProgramID ||
		proof.Pipeline != claim.Pipeline || proof.Kind != claim.Kind ||
		proof.RunRef != claim.RunRef || proof.EvidenceRef != claim.EvidenceRef ||
		proof.SourceRevision != claim.SourceRevision || proof.TargetRevision != claim.TargetRevision {
		return Attestation{}, ErrNoProof
	}
	if claim.Kind == "deploy" && (proof.ImageRef == "" || len(proof.ImageRef) > 512 || !digestPattern.MatchString(proof.ImageID)) {
		return Attestation{}, ErrNoProof
	}
	if len(proof.ImageRef) > 512 || len(proof.ImageID) > 256 {
		return Attestation{}, ErrNoProof
	}
	return proof, nil
}
