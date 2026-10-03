// Package connectorcheck defines the bounded provider-check boundary used by
// the connector registry. Adapters return only fixed health metadata; provider
// response bodies and credentials never cross this package boundary.
package connectorcheck

import (
	"context"
	"strings"
)

// Result is safe to persist as connector health metadata. Adapters must keep
// all fields bounded and use only fixed error codes.
type Result struct {
	Outcome        string
	StatusClass    string
	ErrorCode      string
	QuotaRemaining *int64
	RetryAfterMS   *int64
}

// Probe is the narrow outbound boundary for one fixed-authority provider
// check. The credential is supplied only for this call and must never be
// included in a Result or returned error.
type Probe interface {
	Check(context.Context, string) (Result, error)
}

// Adapter binds one provider token to its fixed operation and outbound probe.
// Operation is metadata only; it is not a URL or a caller-controlled value.
type Adapter struct {
	Operation string
	Probe     Probe
}

// Registry is an immutable provider adapter lookup table. NewServer builds it
// once; callers cannot register arbitrary URLs or providers at runtime.
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry copies the supplied fixed provider map so later caller mutation
// cannot change the active authority set.
func NewRegistry(adapters map[string]Adapter) Registry {
	copyOf := make(map[string]Adapter, len(adapters))
	for provider, adapter := range adapters {
		provider = strings.TrimSpace(provider)
		if provider == "" || adapter.Operation == "" || adapter.Probe == nil {
			continue
		}
		copyOf[provider] = adapter
	}
	return Registry{adapters: copyOf}
}

// Lookup returns the fixed adapter for one canonical provider token.
func (r Registry) Lookup(provider string) (Adapter, bool) {
	adapter, ok := r.adapters[strings.TrimSpace(provider)]
	if !ok || adapter.Probe == nil || adapter.Operation == "" {
		return Adapter{}, false
	}
	return adapter, true
}
