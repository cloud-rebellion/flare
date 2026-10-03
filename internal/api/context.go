package api

import "context"

type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxOrgID
	ctxRole
	ctxAuthSource
	ctxCredentialID
)

func userIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxUserID).(string)
	return v
}

func orgIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxOrgID).(string)
	return v
}

func roleFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRole).(string)
	return v
}

// authSourceFrom identifies the credential boundary that admitted a request.
// It is intentionally a small internal vocabulary; raw bearer tokens, DSNs,
// session IDs, and user identifiers never cross into usage accounting.
func authSourceFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxAuthSource).(string)
	return v
}

// credentialIDFrom returns only an opaque API-key row identifier. Session and
// DSN credentials deliberately have no credential ID in usage buckets.
func credentialIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxCredentialID).(string)
	return v
}

// Role hierarchy. viewer = read-only; member = full project/telemetry writes;
// admin = member + team management; owner = admin + can manage owners and is
// protected from removal/demotion (an org always keeps at least one owner).
var roleRank = map[string]int{"viewer": 0, "member": 1, "admin": 2, "owner": 3}

func validRole(role string) bool {
	_, ok := roleRank[role]
	return ok
}

// roleAtLeast reports whether have is at or above min in the hierarchy.
func roleAtLeast(have, min string) bool {
	return roleRank[have] >= roleRank[min]
}
