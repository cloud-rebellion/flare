package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var errSentryItemIDConflict = errors.New("sentry item identity reused with different payload")

func sentryPayloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func validSentryItemID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_", c) {
			continue
		}
		return false
	}
	return true
}

// claimSentryItem reserves a project-bound identity in the caller's telemetry
// transaction. Unique indexes serialize concurrent replays; a rolled-back
// telemetry transaction rolls back the identity too. Exact payload duplicates
// are already accepted and need no second issue/spans write.
func claimSentryItem(ctx context.Context, tx pgx.Tx, orgID, projectID, itemType, itemID, payloadHash string) (duplicate bool, err error) {
	var identity any
	if itemID != "" {
		identity = itemID
	}
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO sentry_item_acceptances
		(org_id, project_id, item_type, item_id, payload_sha256)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT DO NOTHING RETURNING payload_sha256`, orgID, projectID, itemType, identity, payloadHash).Scan(&inserted)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("reserve sentry item: %w", err)
	}
	var previousHash string
	if itemID != "" {
		err = tx.QueryRow(ctx, `SELECT payload_sha256 FROM sentry_item_acceptances
			WHERE org_id=$1 AND project_id=$2 AND item_type=$3 AND item_id=$4`, orgID, projectID, itemType, itemID).Scan(&previousHash)
		if err == nil {
			if previousHash != payloadHash {
				return false, errSentryItemIDConflict
			}
			return true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("read sentry item identity: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `SELECT payload_sha256 FROM sentry_item_acceptances
		WHERE org_id=$1 AND project_id=$2 AND item_type=$3 AND payload_sha256=$4`, orgID, projectID, itemType, payloadHash).Scan(&previousHash)
	if err != nil {
		return false, fmt.Errorf("read sentry item payload: %w", err)
	}
	return true, nil
}
