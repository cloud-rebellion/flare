// Package ai is a minimal BYOAI client for sovereign issue triage. It calls a
// tenant-configured chat endpoint - OpenAI-compatible (OpenAI, vLLM, Ollama,
// Mistral, ...) or Anthropic Messages - so inference stays where the tenant
// controls it. PII in the prompt is scrubbed by the caller via Scrub.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/netguard"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/bright-interaction/flare/internal/metrics"
)

// Client is an HTTP client for chat completions. When guardSSRF is set (prod),
// outbound requests to loopback/private/link-local addresses are blocked so a
// tenant-supplied base_url cannot reach internal services.
type Client struct {
	http *http.Client
}

func New(guardSSRF bool) *Client {
	tr := &http.Transport{}
	if guardSSRF {
		tr.DialContext = (&net.Dialer{
			Timeout: 15 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if netguard.IsBlockedIP(net.ParseIP(host)) {
					return errors.New("blocked destination address")
				}
				return nil
			},
		}).DialContext
	}
	return &Client{http: &http.Client{
		Timeout:   60 * time.Second,
		Transport: tr,
		// No redirects, matching internal/alerts. Go strips Authorization on a
		// cross-host redirect but does NOT strip custom headers, and the
		// "anthropic" format sends the org's key as x-api-key, so a provider
		// (or anyone who can answer as one) could bounce the request and
		// collect the credential at the next hop. netguard still blocks
		// internal targets per dial, so this is credential leak rather than
		// SSRF, which is why it stayed open: the SSRF fix did not cover it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects disabled")
		},
	}}
}

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	Format  string // "openai" | "anthropic"
}

// Complete sends a system + user prompt and returns the model's reply text.
func (c *Client) Complete(ctx context.Context, cfg Config, system, user string) (reply string, err error) {
	startedAt := time.Now().UTC()
	started := startedAt
	format := "openai"
	if cfg.Format == "anthropic" {
		format = "anthropic"
	}
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("ai", "complete", format, outcome, time.Since(started))
		receiptOutcome, statusClass, errorCode := integration.ClassifyError(err)
		scope := integration.ScopeFrom(ctx)
		integration.Record(ctx, integration.Attempt{
			Scope: scope, Provider: "ai", Operation: "complete", Channel: format,
			Outcome: receiptOutcome, StatusClass: statusClass, ErrorCode: errorCode,
			Elapsed: time.Since(started), AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
	}()
	reqCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	base := strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Format == "anthropic" {
		return c.anthropic(reqCtx, base, cfg, system, user)
	}
	return c.openai(reqCtx, base, cfg, system, user)
}

func (c *Client) openai(ctx context.Context, base string, cfg Config, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       cfg.Model,
		"max_tokens":  1024,
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("ai: unexpected response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func (c *Client) anthropic(ctx context.Context, base string, cfg Config, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"max_tokens": 1024,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || len(out.Content) == 0 {
		return "", fmt.Errorf("ai: unexpected response")
	}
	return strings.TrimSpace(out.Content[0].Text), nil
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ai endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b[:min(200, len(b))])))
	}
	return b, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
