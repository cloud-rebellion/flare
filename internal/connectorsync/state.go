// Package connectorsync provides the provider-neutral worker boundary for
// connector synchronization. It owns operation/lease/cursor bounds and keeps
// cursor encryption at the point where a worker reads or commits state.
package connectorsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/secretbox"
)

const (
	MaxOperationBytes = 64
	MaxWorkerIDBytes  = 128
	MaxCursorBytes    = 14 << 10
	MaxErrorCodeBytes = 64
	MaxLeaseDuration  = time.Hour
)

var errInvalidValue = errors.New("invalid connector sync value")

// State is the safe metadata projection returned by Ensure, Claim, and
// Complete. Cursor material and lease owner tokens never cross this boundary.
type State struct {
	ID               string
	OrgID            string
	ConnectionID     string
	Operation        string
	Status           string
	CursorConfigured bool
	CursorVersion    int64
	AttemptCount     int64
	LastStartedAt    pgtype.Timestamptz
	LastCompletedAt  pgtype.Timestamptz
	LastSuccessAt    pgtype.Timestamptz
	LastErrorCode    string
	LastSyncLagMS    pgtype.Int8
	NextRunAt        pgtype.Timestamptz
	LeaseActive      bool
	LeaseExpiresAt   pgtype.Timestamptz
	CreatedAt        pgtype.Timestamptz
	UpdatedAt        pgtype.Timestamptz
}

// Manager wraps the generated tenant-scoped queries. It is intended for a
// future provider scheduler; no provider implementation is selected here.
type Manager struct {
	q      *generated.Queries
	cipher *secretbox.Cipher
}

func New(q *generated.Queries, cipher *secretbox.Cipher) *Manager {
	return &Manager{q: q, cipher: cipher}
}

func (m *Manager) Ensure(ctx context.Context, orgID, connectionID, operation, stateID string) (State, error) {
	if err := validateIDs(orgID, connectionID); err != nil {
		return State{}, err
	}
	if err := ValidateOperation(operation); err != nil {
		return State{}, err
	}
	if !safePrintable(stateID, 256) {
		return State{}, fmt.Errorf("%w: state id", errInvalidValue)
	}
	row, err := m.q.EnsureConnectorSyncState(ctx, generated.EnsureConnectorSyncStateParams{
		ID: stateID, OrgID: orgID, ConnectionID: connectionID, Operation: operation,
	})
	if err != nil {
		return State{}, err
	}
	return stateFromEnsure(row), nil
}

// Claim atomically acquires a lease only when the connection is active. A
// caller receives pgx.ErrNoRows when another worker owns an unexpired lease.
func (m *Manager) Claim(ctx context.Context, orgID, connectionID, operation, workerID string, leaseFor time.Duration) (State, error) {
	if err := validateIDs(orgID, connectionID); err != nil {
		return State{}, err
	}
	if err := ValidateOperation(operation); err != nil {
		return State{}, err
	}
	if err := ValidateWorkerID(workerID); err != nil {
		return State{}, err
	}
	if leaseFor <= 0 || leaseFor > MaxLeaseDuration {
		return State{}, fmt.Errorf("%w: lease duration", errInvalidValue)
	}
	row, err := m.q.TryClaimConnectorSync(ctx, generated.TryClaimConnectorSyncParams{
		OrgID: orgID, ConnectionID: connectionID, Operation: operation,
		LeaseOwner: workerID, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(leaseFor), Valid: true},
	})
	if err != nil {
		return State{}, err
	}
	return stateFromClaim(row), nil
}

// ClaimDue atomically acquires a lease only when an existing sync state is due.
// It is the scheduler counterpart to Claim: manual runs may be forced by an
// operator, while scheduled runs must respect next_run_at in the same database
// statement that acquires the lease.
func (m *Manager) ClaimDue(ctx context.Context, orgID, connectionID, operation, workerID string, leaseFor time.Duration) (State, error) {
	if err := validateIDs(orgID, connectionID); err != nil {
		return State{}, err
	}
	if err := ValidateOperation(operation); err != nil {
		return State{}, err
	}
	if err := ValidateWorkerID(workerID); err != nil {
		return State{}, err
	}
	if leaseFor <= 0 || leaseFor > MaxLeaseDuration {
		return State{}, fmt.Errorf("%w: lease duration", errInvalidValue)
	}
	row, err := m.q.TryClaimDueConnectorSync(ctx, generated.TryClaimDueConnectorSyncParams{
		OrgID: orgID, ConnectionID: connectionID, Operation: operation,
		LeaseOwner: workerID, LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(leaseFor), Valid: true},
	})
	if err != nil {
		return State{}, err
	}
	return stateFromClaimDue(row), nil
}

// Cursor returns and decrypts the opaque provider cursor at the worker
// boundary. It never returns the encrypted database value to a caller.
func (m *Manager) Cursor(ctx context.Context, orgID, connectionID, operation string) (string, int64, error) {
	if err := validateIDs(orgID, connectionID); err != nil {
		return "", 0, err
	}
	if err := ValidateOperation(operation); err != nil {
		return "", 0, err
	}
	row, err := m.q.GetConnectorSyncCursor(ctx, generated.GetConnectorSyncCursorParams{OrgID: orgID, ConnectionID: connectionID, Operation: operation})
	if err != nil {
		return "", 0, err
	}
	if row.CursorCiphertext == "" {
		return "", row.CursorVersion, nil
	}
	if m.cipher == nil {
		return "", 0, errors.New("connector sync cursor is unavailable")
	}
	cursor, err := m.cipher.Decrypt(row.CursorCiphertext)
	if err != nil {
		return "", 0, errors.New("connector sync cursor is unavailable")
	}
	if err := ValidateCursor(cursor); err != nil {
		return "", 0, errors.New("connector sync cursor is unavailable")
	}
	return cursor, row.CursorVersion, nil
}

// Complete releases the lease and optionally advances the encrypted cursor.
// The generated query refuses completion from a worker that no longer owns
// the lease, preventing late results from overwriting newer state.
func (m *Manager) Complete(ctx context.Context, orgID, connectionID, operation, workerID, outcome, errorCode string, cursor *string, syncLagMS *int64, nextRunAt *time.Time) (State, error) {
	if err := validateIDs(orgID, connectionID); err != nil {
		return State{}, err
	}
	if err := ValidateOperation(operation); err != nil {
		return State{}, err
	}
	if err := ValidateWorkerID(workerID); err != nil {
		return State{}, err
	}
	if outcome != "ok" && outcome != "error" {
		return State{}, fmt.Errorf("%w: outcome", errInvalidValue)
	}
	if outcome == "ok" {
		errorCode = ""
	} else if errorCode == "" || !safeToken(errorCode, MaxErrorCodeBytes) {
		return State{}, fmt.Errorf("%w: error code", errInvalidValue)
	}
	if syncLagMS != nil && (*syncLagMS < 0 || *syncLagMS > int64(7*24*time.Hour/time.Millisecond)) {
		return State{}, fmt.Errorf("%w: sync lag", errInvalidValue)
	}
	cursorCiphertext := ""
	replaceCursor := cursor != nil
	if replaceCursor {
		if err := ValidateCursor(*cursor); err != nil {
			return State{}, err
		}
		if m.cipher == nil {
			return State{}, errors.New("connector sync cursor is unavailable")
		}
		cursorCiphertext = m.cipher.Encrypt(*cursor)
	}
	var lag pgtype.Int8
	if syncLagMS != nil {
		lag = pgtype.Int8{Int64: *syncLagMS, Valid: true}
	}
	var next pgtype.Timestamptz
	if nextRunAt != nil {
		next = pgtype.Timestamptz{Time: nextRunAt.UTC(), Valid: true}
	}
	row, err := m.q.CompleteConnectorSync(ctx, generated.CompleteConnectorSyncParams{
		Outcome: outcome, ReplaceCursor: replaceCursor, CursorCiphertext: cursorCiphertext,
		LastErrorCode: errorCode, LastSyncLagMs: lag, NextRunAt: next,
		OrgID: orgID, ConnectionID: connectionID, Operation: operation, LeaseOwner: workerID,
	})
	if err != nil {
		return State{}, err
	}
	return stateFromComplete(row), nil
}

func ValidateOperation(operation string) error {
	if !safeToken(operation, MaxOperationBytes) {
		return fmt.Errorf("%w: operation", errInvalidValue)
	}
	return nil
}

func ValidateWorkerID(workerID string) error {
	if !safePrintable(workerID, MaxWorkerIDBytes) {
		return fmt.Errorf("%w: worker id", errInvalidValue)
	}
	return nil
}

func ValidateCursor(cursor string) error {
	if cursor == "" || len([]byte(cursor)) > MaxCursorBytes || strings.IndexFunc(cursor, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: cursor", errInvalidValue)
	}
	return nil
}

func validateIDs(orgID, connectionID string) error {
	if !safePrintable(orgID, 256) || !safePrintable(connectionID, 256) {
		return fmt.Errorf("%w: tenant or connection id", errInvalidValue)
	}
	return nil
}

func safeToken(value string, max int) bool {
	if value == "" || len([]byte(value)) > max || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == ':' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func safePrintable(value string, max int) bool {
	return value != "" && len([]byte(value)) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}

func stateFromEnsure(row *generated.EnsureConnectorSyncStateRow) State {
	return State{ID: row.ID, OrgID: row.OrgID, ConnectionID: row.ConnectionID, Operation: row.Operation, Status: row.State,
		CursorConfigured: row.CursorConfigured, CursorVersion: row.CursorVersion, AttemptCount: row.AttemptCount,
		LastStartedAt: row.LastStartedAt, LastCompletedAt: row.LastCompletedAt, LastSuccessAt: row.LastSuccessAt,
		LastErrorCode: row.LastErrorCode, LastSyncLagMS: row.LastSyncLagMs, NextRunAt: row.NextRunAt,
		LeaseActive: row.LeaseActive, LeaseExpiresAt: row.LeaseExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func stateFromClaim(row *generated.TryClaimConnectorSyncRow) State {
	return State{ID: row.ID, OrgID: row.OrgID, ConnectionID: row.ConnectionID, Operation: row.Operation, Status: row.State,
		CursorConfigured: row.CursorConfigured, CursorVersion: row.CursorVersion, AttemptCount: row.AttemptCount,
		LastStartedAt: row.LastStartedAt, LastCompletedAt: row.LastCompletedAt, LastSuccessAt: row.LastSuccessAt,
		LastErrorCode: row.LastErrorCode, LastSyncLagMS: row.LastSyncLagMs, NextRunAt: row.NextRunAt,
		LeaseActive: row.LeaseActive, LeaseExpiresAt: row.LeaseExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func stateFromClaimDue(row *generated.TryClaimDueConnectorSyncRow) State {
	return State{ID: row.ID, OrgID: row.OrgID, ConnectionID: row.ConnectionID, Operation: row.Operation, Status: row.State,
		CursorConfigured: row.CursorConfigured, CursorVersion: row.CursorVersion, AttemptCount: row.AttemptCount,
		LastStartedAt: row.LastStartedAt, LastCompletedAt: row.LastCompletedAt, LastSuccessAt: row.LastSuccessAt,
		LastErrorCode: row.LastErrorCode, LastSyncLagMS: row.LastSyncLagMs, NextRunAt: row.NextRunAt,
		LeaseActive: row.LeaseActive, LeaseExpiresAt: row.LeaseExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func stateFromComplete(row *generated.CompleteConnectorSyncRow) State {
	return State{ID: row.ID, OrgID: row.OrgID, ConnectionID: row.ConnectionID, Operation: row.Operation, Status: row.State,
		CursorConfigured: row.CursorConfigured, CursorVersion: row.CursorVersion, AttemptCount: row.AttemptCount,
		LastStartedAt: row.LastStartedAt, LastCompletedAt: row.LastCompletedAt, LastSuccessAt: row.LastSuccessAt,
		LastErrorCode: row.LastErrorCode, LastSyncLagMS: row.LastSyncLagMs, NextRunAt: row.NextRunAt,
		LeaseActive: row.LeaseActive, LeaseExpiresAt: row.LeaseExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
