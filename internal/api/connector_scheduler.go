package api

import (
	"context"
	"time"
)

// RunConnectorSyncScheduler runs the opt-in connector scheduler. Keeping this
// wrapper on Server makes startup wiring explicit and lets tests construct a
// server without starting background work.
func (s *Server) RunConnectorSyncScheduler(ctx context.Context, interval time.Duration) {
	if s == nil || s.connectorScheduler == nil {
		return
	}
	s.connectorScheduler.Run(ctx, interval)
}
