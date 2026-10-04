package stagereceipt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const testEndpoint = "https://stage.example.test/attestations/verify"
const testSHA = "0123456789abcdef0123456789abcdef01234567"
const testAttestationID = "stgatt_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testClaim() Claim {
	return Claim{ProjectID: "project-1", Kind: "ci", Repository: "repo-1", ProgramID: "flare", Pipeline: "ci-flare", RunRef: "run-1", TargetRevision: testSHA}
}

func testProof() Attestation {
	c := testClaim()
	return Attestation{Verified: true, AttestationID: testAttestationID, ProjectID: c.ProjectID,
		TenantID: "tenant-1", CredentialID: "flare:project-1:v2", Repository: c.Repository,
		ProgramID: c.ProgramID, Pipeline: c.Pipeline, Kind: c.Kind, RunRef: c.RunRef,
		EvidenceRef: c.EvidenceRef, SourceRevision: c.SourceRevision, TargetRevision: c.TargetRevision,
		VerifiedAt: time.Now().UTC()}
}

func testClient(proof Attestation, status int) *Client {
	return &Client{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != testEndpoint ||
			r.Header.Get("Authorization") != "Bearer scoped-token" {
			return nil, errors.New("wrong request")
		}
		var got Claim
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != testClaim() {
			return nil, errors.New("wrong claim")
		}
		body, _ := json.Marshal(proof)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}}
}

func TestVerifyRequiresExactStageProof(t *testing.T) {
	for name, mutate := range map[string]func(*Attestation){
		"verified false": func(p *Attestation) { p.Verified = false },
		"tenant":         func(p *Attestation) { p.TenantID = "other" },
		"credential":     func(p *Attestation) { p.CredentialID = "flare:other" },
		"project":        func(p *Attestation) { p.ProjectID = "other" },
		"repository":     func(p *Attestation) { p.Repository = "other" },
		"program":        func(p *Attestation) { p.ProgramID = "other" },
		"pipeline":       func(p *Attestation) { p.Pipeline = "other" },
		"run":            func(p *Attestation) { p.RunRef = "other" },
		"target":         func(p *Attestation) { p.TargetRevision = "other" },
		"proof id":       func(p *Attestation) { p.AttestationID = "opaque" },
		"timestamp":      func(p *Attestation) { p.VerifiedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			p := testProof()
			mutate(&p)
			_, err := testClient(p, http.StatusOK).Verify(context.Background(), testEndpoint, "scoped-token", "tenant-1", "flare:project-1:v2", testClaim())
			if !errors.Is(err, ErrNoProof) {
				t.Fatalf("mismatch error = %v", err)
			}
		})
	}
	p, err := testClient(testProof(), http.StatusOK).Verify(context.Background(), testEndpoint, "scoped-token", "tenant-1", "flare:project-1:v2", testClaim())
	if err != nil || p.AttestationID != testAttestationID {
		t.Fatalf("exact proof = %#v, %v", p, err)
	}
	if _, err := testClient(testProof(), http.StatusServiceUnavailable).Verify(context.Background(), testEndpoint, "scoped-token", "tenant-1", "flare:project-1:v2", testClaim()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable Stage error = %v", err)
	}
}

func TestVerifyDeployRequiresImageIdentity(t *testing.T) {
	c := testClaim()
	c.Kind = "deploy"
	c.SourceRevision = c.TargetRevision
	c.Pipeline = "deploy-flare"
	c.EvidenceRef = strings.Repeat("a", 64)
	p := testProof()
	p.Kind = c.Kind
	p.SourceRevision = c.SourceRevision
	p.Pipeline = c.Pipeline
	p.EvidenceRef = c.EvidenceRef
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(p)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}}
	if _, err := client.Verify(context.Background(), testEndpoint, "scoped-token", "tenant-1", "flare:project-1:v2", c); !errors.Is(err, ErrNoProof) {
		t.Fatalf("missing image accepted: %v", err)
	}
	p.ImageRef = "registry.example/flare:release"
	p.ImageID = "sha256:" + strings.Repeat("b", 64)
	if _, err := client.Verify(context.Background(), testEndpoint, "scoped-token", "tenant-1", "flare:project-1:v2", c); err != nil {
		t.Fatalf("valid deploy proof rejected: %v", err)
	}
}

func TestPinnedStagePeerKeepsHTTPSAuthority(t *testing.T) {
	if err := ValidateEndpoint("https://stage.cloudrebellion.tech/attestations/verify"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://stage.cloudrebellion.tech/attestations/verify", "https://127.0.0.1/attestations/verify", "https://stage.cloudrebellion.tech:8443/attestations/verify", "https://stage.cloudrebellion.tech/other", "https://u:p@stage.cloudrebellion.tech/attestations/verify"} {
		if ValidateEndpoint(bad) == nil {
			t.Fatalf("accepted unsafe endpoint %q", bad)
		}
	}
	client := NewClient("100.118.61.16")
	if client.invalidPin || !client.peerIP.Equal(net.ParseIP("100.118.61.16")) {
		t.Fatal("exact peer pin missing")
	}
	target, err := pinnedDialTarget("stage.cloudrebellion.tech:443", client.peerIP)
	if err != nil || target != "100.118.61.16:443" {
		t.Fatalf("pinned target = %q, %v", target, err)
	}
	if _, err := pinnedDialTarget("other.example:8443", client.peerIP); err == nil {
		t.Fatal("alternate port accepted")
	}
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig != nil || client.http.CheckRedirect == nil {
		t.Fatal("pinned peer must retain default TLS hostname verification, no proxy and no redirects")
	}
	if !NewClient("not-an-ip").invalidPin {
		t.Fatal("invalid pin accepted")
	}
	if ValidatePeerIP("100.118.61.16") != nil || ValidatePeerIP("") != nil || ValidatePeerIP("not-an-ip") == nil {
		t.Fatal("operator peer pin validation is not exact")
	}
}
