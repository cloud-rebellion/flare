// Package github opens issues on github.com via the REST API. The host is
// fixed (api.github.com), so there is no SSRF surface; only the repo path and
// the org's own token vary.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bright-interaction/flare/internal/integration"
	"github.com/bright-interaction/flare/internal/metrics"
)

var httpClient = &http.Client{Timeout: 12 * time.Second}

// ValidRepo reports whether repo looks like "owner/name".
func ValidRepo(repo string) bool {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// CreateIssue opens an issue in owner/name and returns its html_url.
func CreateIssue(ctx context.Context, token, repo, title, body string) (htmlURL string, err error) {
	startedAt := time.Now().UTC()
	started := startedAt
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metrics.RecordIntegration("github", "create_issue", "http", outcome, time.Since(started))
		receiptOutcome, statusClass, errorCode := integration.ClassifyError(err)
		integration.Record(ctx, integration.Attempt{
			Scope: integration.ScopeFrom(ctx), Provider: "github", Operation: "create_issue", Channel: "http",
			Outcome: receiptOutcome, StatusClass: statusClass, ErrorCode: errorCode,
			Elapsed: time.Since(started), AttemptedAt: startedAt, CompletedAt: time.Now().UTC(),
		})
	}()
	payload, err := json.Marshal(map[string]string{"title": title, "body": body})
	if err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "https://api.github.com/repos/"+repo+"/issues", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("github returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.HTMLURL == "" {
		return "", fmt.Errorf("github: could not read created issue url")
	}
	return out.HTMLURL, nil
}
