package partition

import (
	"context"
	"time"
)

// Test-only hooks for the external partition_test package, which drives real
// passes against the real analytics exporter (analytics imports partition, so
// those tests cannot live in this package).

func (m *Manager) SetClockForTest(now func() time.Time) { m.now = now }

func (m *Manager) OnceForTest(ctx context.Context) { m.once(ctx) }
