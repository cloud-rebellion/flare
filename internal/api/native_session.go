package api

import (
	"context"
	"errors"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/jackc/pgx/v5"
)

var errNativeSessionRequired = errors.New("native session required")
var errNativeSessionUnavailable = errors.New("native session unavailable")

// nativeSessionUser is the common browser-session probe for /me, protected
// routes and direct SSO launch. Local auth_epoch remains revocation state,
// never evidence about the provider's authentication time or method.
func (s *Server) nativeSessionUser(ctx context.Context) (*generated.User, error) {
	if s.sessions == nil {
		return nil, errNativeSessionUnavailable
	}
	uid := s.sessions.GetString(ctx, "user_id")
	if uid == "" {
		return nil, errNativeSessionRequired
	}
	if s.q == nil {
		return nil, errNativeSessionUnavailable
	}
	user, err := s.q.GetUserByID(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNativeSessionRequired
	}
	if err != nil {
		return nil, err
	}
	epoch := s.sessions.GetInt64(ctx, "auth_epoch")
	if epoch > 0 && user.SessionsValidFrom.Valid && user.SessionsValidFrom.Time.Unix() > epoch {
		_ = s.sessions.Destroy(ctx)
		return nil, errNativeSessionRequired
	}
	return user, nil
}
