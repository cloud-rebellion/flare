package automationoutbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrDeliveryPage = errors.New("invalid automation delivery pagination")

type DeliveryPage struct {
	Items      []DeliveryReceipt `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// The cursor is an opaque ordering bookmark, not an authorization capability.
// Its scope fingerprint rejects accidental reuse with another tenant, project
// or issue filter; every query independently enforces the caller's scope.
type deliveryCursor struct {
	Version int       `json:"v"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
	Scope   string    `json:"scope"`
}

func deliveryScope(org, project, issue string) string {
	raw, _ := json.Marshal([3]string{org, project, issue})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseDeliveryCursor(raw, scope string) (deliveryCursor, error) {
	var c deliveryCursor
	if raw == "" || len(raw) > 512 {
		return c, ErrDeliveryPage
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || !utf8.Valid(data) {
		return c, ErrDeliveryPage
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(&struct{}{}) != io.EOF || c.Version != 1 || c.Scope != scope || c.At.IsZero() || c.At.Year() < 1 || c.At.Year() > 9999 || len(c.ID) == 0 || len(c.ID) > 128 || strings.ContainsRune(c.ID, '\x00') {
		return deliveryCursor{}, ErrDeliveryPage
	}
	return c, nil
}

// ListDeliveryReceipts uses a stable newest-first (created_at,id) keyset. It
// reads at most limit+1 rows and retains no destination or response payload.
func (s *Store) ListDeliveryReceipts(ctx context.Context, org, project, issue string, limit int, cursor string) (DeliveryPage, error) {
	page := DeliveryPage{Items: []DeliveryReceipt{}}
	if limit == 0 {
		limit = 25
	}
	if org == "" || project == "" || limit < 1 || limit > 100 || len(issue) > 128 || !utf8.ValidString(issue) || strings.ContainsRune(issue, '\x00') {
		return page, ErrDeliveryPage
	}
	scope := deliveryScope(org, project, issue)
	query := `SELECT id,channel_id,issue_id,event,event_count,observed_at,state,attempts,
        receiver_mode,receiver_http_status,receiver_request_sha256,receiver_run_id,receiver_deduped,receiver_acknowledged_at,created_at
        FROM automation_alert_outbox WHERE org_id=$1 AND project_id=$2`
	args := []any{org, project}
	if issue != "" {
		args = append(args, issue)
		query += fmt.Sprintf(" AND issue_id=$%d", len(args))
	}
	if cursor != "" {
		before, err := parseDeliveryCursor(cursor, scope)
		if err != nil {
			return page, err
		}
		args = append(args, before.At, before.ID)
		query += fmt.Sprintf(" AND (created_at,id)<($%d::timestamptz,$%d::text)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last DeliveryReceipt
	var lastAt time.Time
	for rows.Next() {
		var at time.Time
		r, err := scanDeliveryReceipt(rows, &at)
		if err != nil {
			return page, err
		}
		if len(page.Items) == limit {
			raw, err := json.Marshal(deliveryCursor{1, lastAt, last.NoticeID, scope})
			if err != nil {
				return page, err
			}
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		page.Items = append(page.Items, r)
		last, lastAt = r, at
	}
	return page, rows.Err()
}
