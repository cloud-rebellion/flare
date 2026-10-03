package api

import "time"

// healthConnectorReceipts is a process-level projection of the connector
// receipt writer. It deliberately has no tenant, provider, credential, or
// payload fields, so it is safe to include in the unauthenticated readiness
// response. "healthy" means the latest attempted write completed; it does not
// claim that every historical write succeeded.
type healthConnectorReceipts struct {
	Configured    bool       `json:"configured"`
	State         string     `json:"state"` // unobserved | healthy | degraded
	Durable       bool       `json:"durable"`
	Started       uint64     `json:"started"`
	Completed     uint64     `json:"completed"`
	Succeeded     uint64     `json:"succeeded"`
	Failed        uint64     `json:"failed"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
}

func (s *Server) observeConnectorReceipt(success bool) {
	now := time.Now().UTC()
	s.connectorReceiptMu.Lock()
	defer s.connectorReceiptMu.Unlock()
	s.connectorReceiptCompleted++
	if success {
		s.connectorReceiptSucceeded++
		s.connectorReceiptState = "healthy"
		s.connectorReceiptLastOK = now
		return
	}
	s.connectorReceiptFailed++
	s.connectorReceiptState = "degraded"
	s.connectorReceiptLastError = now
}

func (s *Server) connectorReceiptHealthSnapshot() healthConnectorReceipts {
	s.connectorReceiptMu.RLock()
	defer s.connectorReceiptMu.RUnlock()
	out := healthConnectorReceipts{
		Configured: s.connectorReceiptConfigured,
		State:      s.connectorReceiptState,
		Durable:    s.connectorReceiptState == "healthy",
		Started:    s.connectorReceiptStarted,
		Completed:  s.connectorReceiptCompleted,
		Succeeded:  s.connectorReceiptSucceeded,
		Failed:     s.connectorReceiptFailed,
	}
	if out.State == "" {
		out.State = "unobserved"
	}
	out.Durable = out.State == "healthy"
	if !s.connectorReceiptLastOK.IsZero() {
		v := s.connectorReceiptLastOK
		out.LastSuccessAt = &v
	}
	if !s.connectorReceiptLastError.IsZero() {
		v := s.connectorReceiptLastError
		out.LastFailureAt = &v
	}
	return out
}
