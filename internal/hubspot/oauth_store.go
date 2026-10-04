package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bright-interaction/flare/internal/secretbox"
)

const credentialPrefix = "flare:hubspot:oauth:v1:"

// OAuthCredential is an encrypted, server-owned value in the connector's
// secret_ciphertext column. Scope fields prevent ciphertext copied between
// connection rows from being used in another workspace or project.
type OAuthCredential struct {
	OrgID        string    `json:"org_id"`
	ProjectID    string    `json:"project_id"`
	ConnectionID string    `json:"connection_id"`
	HubID        int64     `json:"hub_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func NewOAuthCredential(orgID, projectID, connectionID string, tokens OAuthTokens, now time.Time) OAuthCredential {
	return OAuthCredential{
		OrgID: orgID, ProjectID: projectID, ConnectionID: connectionID,
		HubID: tokens.HubID, AccessToken: tokens.AccessToken,
		RefreshToken: tokens.RefreshToken, ExpiresAt: now.UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second),
	}
}

func (c OAuthCredential) Encode(cipher *secretbox.Cipher) (string, error) {
	if cipher == nil || !cipher.Enabled() || c.OrgID == "" || c.ProjectID == "" || c.ConnectionID == "" ||
		c.HubID <= 0 || c.AccessToken == "" || c.RefreshToken == "" || c.ExpiresAt.IsZero() {
		return "", errors.New("hubspot oauth credential encryption is unavailable")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", errors.New("hubspot oauth credential is invalid")
	}
	stored := cipher.Encrypt(credentialPrefix + string(raw))
	if !strings.HasPrefix(stored, "enc:v1:") {
		return "", errors.New("hubspot oauth credential encryption failed")
	}
	return stored, nil
}

// DecodeOAuthCredential distinguishes a legacy/manual bearer token from a
// structured OAuth grant. Callers must validate the bound scope before use.
func DecodeOAuthCredential(cipher *secretbox.Cipher, ciphertext string) (OAuthCredential, string, bool, error) {
	if cipher == nil {
		return OAuthCredential{}, "", false, errors.New("hubspot credential cipher is unavailable")
	}
	if strings.HasPrefix(ciphertext, "enc:v1:") && !cipher.Enabled() {
		return OAuthCredential{}, "", false, errors.New("hubspot credential cipher is unavailable")
	}
	plain, err := cipher.Decrypt(ciphertext)
	if err != nil {
		return OAuthCredential{}, "", false, errors.New("hubspot credential cannot be decrypted")
	}
	if !strings.HasPrefix(plain, credentialPrefix) {
		return OAuthCredential{}, plain, false, nil
	}
	var cred OAuthCredential
	if err := json.Unmarshal([]byte(strings.TrimPrefix(plain, credentialPrefix)), &cred); err != nil ||
		cred.OrgID == "" || cred.ProjectID == "" || cred.ConnectionID == "" || cred.HubID <= 0 ||
		cred.AccessToken == "" || cred.RefreshToken == "" || cred.ExpiresAt.IsZero() {
		return OAuthCredential{}, "", true, errors.New("hubspot oauth credential is invalid")
	}
	return cred, "", true, nil
}

// TokenStore refreshes short-lived OAuth grants before a probe. The row lock
// serializes refreshes across workers and check requests, so a rotated refresh
// token cannot be used concurrently. It also serializes with disconnect.
type TokenStore struct {
	Pool   *pgxpool.Pool
	Cipher *secretbox.Cipher
	Client OAuthClient
	Now    func() time.Time
}

func (s TokenStore) Resolve(ctx context.Context, orgID, connectionID string) (string, error) {
	if s.Pool == nil || s.Cipher == nil {
		return "", errors.New("hubspot token store is unavailable")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", errors.New("hubspot token store is unavailable")
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var provider, status, installationID, projectID, ciphertext string
	err = tx.QueryRow(ctx, `SELECT provider, status, installation_id, COALESCE(project_id,''), secret_ciphertext
        FROM connector_connections WHERE id=$1 AND org_id=$2 FOR UPDATE`, connectionID, orgID).
		Scan(&provider, &status, &installationID, &projectID, &ciphertext)
	if err != nil || provider != "hubspot" || status == "paused" || status == "revoked" {
		return "", errors.New("hubspot connection is unavailable")
	}
	cred, manual, isOAuth, err := DecodeOAuthCredential(s.Cipher, ciphertext)
	if err != nil {
		return "", err
	}
	if !isOAuth {
		if err := tx.Commit(ctx); err != nil {
			return "", errors.New("hubspot token store is unavailable")
		}
		return manual, nil
	}
	if cred.OrgID != orgID || cred.ConnectionID != connectionID || cred.ProjectID != projectID ||
		strconv.FormatInt(cred.HubID, 10) != installationID {
		return "", errors.New("hubspot oauth credential scope mismatch")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if cred.ExpiresAt.After(now.Add(2 * time.Minute)) {
		if err := tx.Commit(ctx); err != nil {
			return "", errors.New("hubspot token store is unavailable")
		}
		return cred.AccessToken, nil
	}
	tokens, err := s.Client.Refresh(ctx, cred.RefreshToken)
	if err != nil {
		return "", err
	}
	if tokens.HubID != cred.HubID {
		return "", errors.New("hubspot refreshed token changed account")
	}
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "oauth" {
		return "", errors.New("hubspot refreshed token changed scope")
	}
	cred.AccessToken, cred.RefreshToken = tokens.AccessToken, tokens.RefreshToken
	cred.ExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	stored, err := cred.Encode(s.Cipher)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE connector_connections SET secret_ciphertext=$3, updated_at=now()
        WHERE id=$1 AND org_id=$2`, connectionID, orgID, stored); err != nil {
		return "", errors.New("hubspot refreshed token could not be saved")
	}
	if err := tx.Commit(ctx); err != nil {
		return "", errors.New("hubspot refreshed token could not be saved")
	}
	return cred.AccessToken, nil
}

// ReadOAuthForDisconnect locks the connector, disables it, and returns its
// bound refresh token. The caller revokes with HubSpot and then deletes the
// local row. If the provider is down the disabled row remains retryable.
func (s TokenStore) DisableForDisconnect(ctx context.Context, orgID, connectionID string) (string, bool, error) {
	if s.Pool == nil || s.Cipher == nil {
		return "", false, errors.New("hubspot token store is unavailable")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", false, errors.New("hubspot token store is unavailable")
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var provider, installationID, projectID, ciphertext string
	err = tx.QueryRow(ctx, `SELECT provider, installation_id, COALESCE(project_id,''), secret_ciphertext
        FROM connector_connections WHERE id=$1 AND org_id=$2 FOR UPDATE`, connectionID, orgID).
		Scan(&provider, &installationID, &projectID, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, pgx.ErrNoRows
	}
	if err != nil {
		return "", false, errors.New("hubspot connection is unavailable")
	}
	if provider != "hubspot" {
		return "", false, nil
	}
	cred, _, isOAuth, err := DecodeOAuthCredential(s.Cipher, ciphertext)
	if err != nil {
		return "", false, err
	}
	if !isOAuth {
		return "", false, nil
	}
	if cred.OrgID != orgID || cred.ProjectID != projectID || cred.ConnectionID != connectionID ||
		strconv.FormatInt(cred.HubID, 10) != installationID {
		return "", true, errors.New("hubspot oauth credential scope mismatch")
	}
	if _, err := tx.Exec(ctx, `UPDATE connector_connections SET status='revoked', updated_at=now()
        WHERE id=$1 AND org_id=$2`, connectionID, orgID); err != nil {
		return "", true, errors.New("hubspot connection could not be disabled")
	}
	if err := tx.Commit(ctx); err != nil {
		return "", true, errors.New("hubspot connection could not be disabled")
	}
	return cred.RefreshToken, true, nil
}
