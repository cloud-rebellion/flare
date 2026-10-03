package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bright-interaction/flare/internal/connectorcheck"
	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
	"github.com/bright-interaction/flare/internal/integration"
)

const (
	maxConnectorInstallationID   = 128
	maxConnectorInstallationName = 128
	maxConnectorScopes           = 64
	maxConnectorScopeBytes       = 128
	maxConnectorSecretBytes      = 16 << 10
)

var connectorScopeToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+\-]{0,127}$`)

var connectorConnectionStatuses = map[string]struct{}{
	"pending": {},
	"active":  {},
	"paused":  {},
	"error":   {},
	"revoked": {},
}

// connectorConnectionResponse is deliberately metadata-only. In particular,
// it does not contain the encrypted column or a decrypted credential. The
// registry is safe to render in an admin dashboard and to return from a list
// endpoint without turning a read into a credential export.
type connectorConnectionResponse struct {
	ID                   string   `json:"id"`
	Provider             string   `json:"provider"`
	InstallationID       string   `json:"installation_id"`
	InstallationName     string   `json:"installation_name"`
	Status               string   `json:"status"`
	Scopes               []string `json:"scopes"`
	CredentialConfigured bool     `json:"credential_configured"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

type connectorConnectionCreateRequest struct {
	Provider         string   `json:"provider"`
	InstallationID   string   `json:"installation_id"`
	InstallationName string   `json:"installation_name"`
	Status           string   `json:"status"`
	Scopes           []string `json:"scopes"`
	Secret           *string  `json:"secret"`
}

type connectorConnectionPatchRequest struct {
	Provider         *string   `json:"provider"`
	InstallationID   *string   `json:"installation_id"`
	InstallationName *string   `json:"installation_name"`
	Status           *string   `json:"status"`
	Scopes           *[]string `json:"scopes"`
	Secret           *string   `json:"secret"`
}

// connectorProbe is the narrow outbound boundary used by an on-demand
// connection check. Keeping the dependency behind this interface lets tests
// exercise the API without opening a listener or contacting a provider.
type connectorProbe = connectorcheck.Probe

type connectorCheckResponse struct {
	Provider       string `json:"provider"`
	Operation      string `json:"operation"`
	Outcome        string `json:"outcome"`
	StatusClass    string `json:"status_class"`
	ErrorCode      string `json:"error_code,omitempty"`
	QuotaRemaining *int64 `json:"quota_remaining,omitempty"`
	RetryAfterMS   *int64 `json:"retry_after_ms,omitempty"`
}

type connectorConnectionValues struct {
	provider         string
	installationID   string
	installationName string
	status           string
	scopes           json.RawMessage
}

func normalizeConnectorConnectionValues(provider, installationID, installationName, status string, scopes []string) (connectorConnectionValues, error) {
	provider = strings.TrimSpace(provider)
	if !connectorProviderToken.MatchString(provider) {
		return connectorConnectionValues{}, fmt.Errorf("provider must be 1-32 letters, numbers, dot, underscore, colon, or hyphen")
	}
	installationID = strings.TrimSpace(installationID)
	if installationID == "" || len([]byte(installationID)) > maxConnectorInstallationID || strings.IndexFunc(installationID, unicode.IsControl) >= 0 {
		return connectorConnectionValues{}, fmt.Errorf("installation_id must be 1-%d printable bytes", maxConnectorInstallationID)
	}
	installationName = strings.TrimSpace(installationName)
	if len([]byte(installationName)) > maxConnectorInstallationName || strings.IndexFunc(installationName, unicode.IsControl) >= 0 {
		return connectorConnectionValues{}, fmt.Errorf("installation_name must be at most %d printable bytes", maxConnectorInstallationName)
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "pending"
	}
	if _, ok := connectorConnectionStatuses[status]; !ok {
		return connectorConnectionValues{}, fmt.Errorf("status must be pending, active, paused, error, or revoked")
	}
	if len(scopes) > maxConnectorScopes {
		return connectorConnectionValues{}, fmt.Errorf("at most %d scopes are allowed", maxConnectorScopes)
	}
	seen := make(map[string]struct{}, len(scopes))
	canonicalScopes := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if len([]byte(scope)) == 0 || len([]byte(scope)) > maxConnectorScopeBytes || !connectorScopeToken.MatchString(scope) {
			return connectorConnectionValues{}, fmt.Errorf("each scope must be 1-%d token bytes", maxConnectorScopeBytes)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		canonicalScopes = append(canonicalScopes, scope)
	}
	scopeJSON, err := json.Marshal(canonicalScopes)
	if err != nil {
		return connectorConnectionValues{}, fmt.Errorf("encode scopes: %w", err)
	}
	return connectorConnectionValues{
		provider: provider, installationID: installationID, installationName: installationName,
		status: status, scopes: scopeJSON,
	}, nil
}

func normalizeConnectorSecret(secret *string) (string, bool, error) {
	if secret == nil {
		return "", false, nil
	}
	if strings.TrimSpace(*secret) == "" {
		return "", true, fmt.Errorf("secret cannot be empty; omit it to keep the existing credential")
	}
	if len([]byte(*secret)) > maxConnectorSecretBytes || strings.IndexFunc(*secret, unicode.IsControl) >= 0 {
		return "", true, fmt.Errorf("secret must be printable and at most %d bytes", maxConnectorSecretBytes)
	}
	return *secret, true, nil
}

func connectorScopes(raw json.RawMessage) []string {
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func connectorConnectionTimestamp(ts time.Time, valid bool) string {
	if !valid {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func connectorConnectionResponseFromValues(id, provider, installationID, installationName, status string, scopes json.RawMessage, credentialConfigured bool, createdAt, updatedAt time.Time, createdValid, updatedValid bool) connectorConnectionResponse {
	return connectorConnectionResponse{
		ID: id, Provider: provider, InstallationID: installationID, InstallationName: installationName,
		Status: status, Scopes: connectorScopes(scopes), CredentialConfigured: credentialConfigured,
		CreatedAt: connectorConnectionTimestamp(createdAt, createdValid), UpdatedAt: connectorConnectionTimestamp(updatedAt, updatedValid),
	}
}

func connectorConnectionResponseFromCreate(row *generated.CreateConnectorConnectionRow) connectorConnectionResponse {
	return connectorConnectionResponseFromValues(row.ID, row.Provider, row.InstallationID, row.InstallationName, row.Status, row.Scopes, row.CredentialConfigured, row.CreatedAt.Time, row.UpdatedAt.Time, row.CreatedAt.Valid, row.UpdatedAt.Valid)
}

func connectorConnectionResponseFromList(row *generated.ListConnectorConnectionsByOrgRow) connectorConnectionResponse {
	return connectorConnectionResponseFromValues(row.ID, row.Provider, row.InstallationID, row.InstallationName, row.Status, row.Scopes, row.CredentialConfigured, row.CreatedAt.Time, row.UpdatedAt.Time, row.CreatedAt.Valid, row.UpdatedAt.Valid)
}

func connectorConnectionResponseFromGet(row *generated.GetConnectorConnectionByIDRow) connectorConnectionResponse {
	return connectorConnectionResponseFromValues(row.ID, row.Provider, row.InstallationID, row.InstallationName, row.Status, row.Scopes, row.CredentialConfigured, row.CreatedAt.Time, row.UpdatedAt.Time, row.CreatedAt.Valid, row.UpdatedAt.Valid)
}

func connectorConnectionResponseFromUpdate(row *generated.UpdateConnectorConnectionRow) connectorConnectionResponse {
	return connectorConnectionResponseFromValues(row.ID, row.Provider, row.InstallationID, row.InstallationName, row.Status, row.Scopes, row.CredentialConfigured, row.CreatedAt.Time, row.UpdatedAt.Time, row.CreatedAt.Valid, row.UpdatedAt.Valid)
}

func (s *Server) handleListConnectorConnections(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListConnectorConnectionsByOrg(r.Context(), orgIDFrom(r.Context()))
	if err != nil {
		slogError(w, "list connector connections", err)
		return
	}
	out := make([]connectorConnectionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectorConnectionResponseFromList(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetConnectorConnection(w http.ResponseWriter, r *http.Request) {
	row, err := s.q.GetConnectorConnectionByID(r.Context(), generated.GetConnectorConnectionByIDParams{
		ID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	if err != nil {
		slogError(w, "get connector connection", err)
		return
	}
	writeJSON(w, http.StatusOK, connectorConnectionResponseFromGet(row))
}

func (s *Server) handleCreateConnectorConnection(w http.ResponseWriter, r *http.Request) {
	var req connectorConnectionCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	values, err := normalizeConnectorConnectionValues(req.Provider, req.InstallationID, req.InstallationName, req.Status, req.Scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	secret, _, err := normalizeConnectorSecret(req.Secret)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if values.status == "active" && secret == "" {
		writeErr(w, http.StatusBadRequest, "an active connection requires a secret")
		return
	}
	row, err := s.q.CreateConnectorConnection(r.Context(), generated.CreateConnectorConnectionParams{
		ID: id.New(), OrgID: orgIDFrom(r.Context()), Provider: values.provider,
		InstallationID: values.installationID, InstallationName: values.installationName,
		Status: values.status, Scopes: values.scopes, SecretCiphertext: s.secrets.Encrypt(secret),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "a connection for this provider and installation already exists")
			return
		}
		slogError(w, "create connector connection", err)
		return
	}
	s.audit(r.Context(), "connector.connect", row.ID)
	writeJSON(w, http.StatusCreated, connectorConnectionResponseFromCreate(row))
}

func (s *Server) handleUpdateConnectorConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	idParam := chi.URLParam(r, "id")
	current, err := s.q.GetConnectorConnectionByID(ctx, generated.GetConnectorConnectionByIDParams{ID: idParam, OrgID: org})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	if err != nil {
		slogError(w, "get connector connection", err)
		return
	}
	var req connectorConnectionPatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provider, installationID, installationName, status, scopes := current.Provider, current.InstallationID, current.InstallationName, current.Status, connectorScopes(current.Scopes)
	if req.Provider != nil {
		provider = *req.Provider
	}
	if req.InstallationID != nil {
		installationID = *req.InstallationID
	}
	if req.InstallationName != nil {
		installationName = *req.InstallationName
	}
	if req.Status != nil {
		status = *req.Status
	}
	if req.Scopes != nil {
		scopes = *req.Scopes
	}
	values, err := normalizeConnectorConnectionValues(provider, installationID, installationName, status, scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	secret, supplied, err := normalizeConnectorSecret(req.Secret)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if supplied {
		secret = s.secrets.Encrypt(secret)
	}
	// Omitted secret is an explicit keep-existing operation in SQL, so a
	// metadata-only PATCH cannot erase or race away a stored credential. An
	// active row still needs either the existing credential or a replacement.
	if values.status == "active" && !current.CredentialConfigured && !supplied {
		writeErr(w, http.StatusBadRequest, "an active connection requires a secret")
		return
	}
	row, err := s.q.UpdateConnectorConnection(ctx, generated.UpdateConnectorConnectionParams{
		ID: idParam, OrgID: org, Provider: values.provider, InstallationID: values.installationID,
		InstallationName: values.installationName, Status: values.status, Scopes: values.scopes,
		SecretCiphertext: secret, ReplaceSecret: supplied,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "a connection for this provider and installation already exists")
			return
		}
		slogError(w, "update connector connection", err)
		return
	}
	s.audit(ctx, "connector.update", row.ID)
	writeJSON(w, http.StatusOK, connectorConnectionResponseFromUpdate(row))
}

func (s *Server) handleDeleteConnectorConnection(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.DeleteConnectorConnection(r.Context(), generated.DeleteConnectorConnectionParams{ID: chi.URLParam(r, "id"), OrgID: orgIDFrom(r.Context())})
	if err != nil {
		slogError(w, "delete connector connection", err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	s.audit(r.Context(), "connector.disconnect", chi.URLParam(r, "id"))
	writeJSON(w, http.StatusNoContent, nil)
}

// handleCheckConnectorConnection performs one bounded, provider-specific
// health check. The credential is read only at this boundary and the provider
// adapter records a payload-free attempt using the authenticated org scope.
// A check never changes the lifecycle state chosen by an administrator.
func (s *Server) handleCheckConnectorConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	connectionID := chi.URLParam(r, "id")
	row, err := s.q.GetConnectorConnectionByID(ctx, generated.GetConnectorConnectionByIDParams{ID: connectionID, OrgID: org})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	if err != nil {
		slogError(w, "get connector connection for health check", err)
		return
	}
	if row.Status == "paused" || row.Status == "revoked" {
		writeErr(w, http.StatusConflict, "connector connection is not enabled for checks")
		return
	}
	adapter, ok := s.connectorProbeFor(row.Provider)
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "health checks are not supported for this provider")
		return
	}
	if !row.CredentialConfigured {
		writeErr(w, http.StatusBadRequest, "connector connection has no credential")
		return
	}
	ciphertext, err := s.q.GetConnectorConnectionSecret(ctx, generated.GetConnectorConnectionSecretParams{ID: connectionID, OrgID: org})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "connector connection not found")
		return
	}
	if err != nil {
		slogError(w, "get connector connection credential", err)
		return
	}
	if s.secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, "connector credential is unavailable")
		return
	}
	secret, err := s.secrets.Decrypt(ciphertext)
	if err != nil {
		slogError(w, "decrypt connector connection credential", err)
		writeErr(w, http.StatusServiceUnavailable, "connector credential is unavailable")
		return
	}
	result, probeErr := adapter.Probe.Check(integration.WithScope(ctx, integration.Scope{OrgID: org, ConnectionID: connectionID}), secret)
	if probeErr != nil && result.Outcome == "" {
		result.Outcome, result.StatusClass, result.ErrorCode = integration.ClassifyError(probeErr)
	}
	s.audit(ctx, "connector.check", connectionID)
	writeJSON(w, http.StatusOK, connectorCheckResponse{
		Provider: row.Provider, Operation: adapter.Operation, Outcome: result.Outcome,
		StatusClass: result.StatusClass, ErrorCode: result.ErrorCode,
		QuotaRemaining: result.QuotaRemaining, RetryAfterMS: result.RetryAfterMS,
	})
}

// connectorProbeFor resolves only adapters compiled into the server. HubSpot
// remains injectable for tests and is kept separate from the sync worker; Slack
// and OpenAI checks are on-demand probes only and never enter scheduler state.
func (s *Server) connectorProbeFor(provider string) (connectorcheck.Adapter, bool) {
	if provider == "hubspot" && s.hubspotProbe != nil {
		return connectorcheck.Adapter{Operation: "account_details", Probe: s.hubspotProbe}, true
	}
	return s.connectorProbes.Lookup(provider)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
