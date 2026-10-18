package automationoutbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeliveryCursorRefusesMalformedAndForeignBookmarksBeforeQuery(t *testing.T) {
	scope := deliveryScope("org-a", "project-a", "issue-a")
	valid := deliveryCursor{1, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), "notice-a", scope}
	encode := func(c deliveryCursor) string {
		raw, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	if got, err := parseDeliveryCursor(encode(valid), scope); err != nil || got.ID != valid.ID {
		t.Fatalf("valid bookmark refused: %+v %v", got, err)
	}
	for _, foreign := range []string{deliveryScope("org-b", "project-a", "issue-a"), deliveryScope("org-a", "project-b", "issue-a"), deliveryScope("org-a", "project-a", "issue-b")} {
		if _, err := parseDeliveryCursor(encode(valid), foreign); !errors.Is(err, ErrDeliveryPage) {
			t.Fatalf("foreign scope accepted: %v", err)
		}
	}
	for _, raw := range []string{"%%%", strings.Repeat("a", 513), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`)), encode(deliveryCursor{2, valid.At, valid.ID, scope}), encode(deliveryCursor{1, time.Time{}, valid.ID, scope}), encode(deliveryCursor{1, valid.At, "", scope}), encode(deliveryCursor{1, valid.At, "bad\x00id", scope})} {
		// A nil pool makes an accidental database query fail the test; all
		// malformed bookmarks must refuse before any database access.
		if _, err := New(nil).ListDeliveryReceipts(context.Background(), "org-a", "project-a", "issue-a", 25, raw); !errors.Is(err, ErrDeliveryPage) {
			t.Fatalf("malformed bookmark accepted: %v", err)
		}
	}
}
