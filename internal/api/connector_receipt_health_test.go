package api

import "testing"

func TestConnectorReceiptHealthProjectionTracksDurabilityWithoutTenantData(t *testing.T) {
	s := &Server{connectorReceiptConfigured: true, connectorReceiptState: "unobserved"}
	before := s.connectorReceiptHealthSnapshot()
	if before.State != "unobserved" || before.Durable || before.Started != 0 || before.Completed != 0 {
		t.Fatalf("initial receipt health = %#v", before)
	}

	s.connectorReceiptMu.Lock()
	s.connectorReceiptStarted++
	s.connectorReceiptMu.Unlock()
	s.observeConnectorReceipt(true)
	healthy := s.connectorReceiptHealthSnapshot()
	if healthy.State != "healthy" || !healthy.Durable || healthy.Started != 1 || healthy.Completed != 1 || healthy.Succeeded != 1 || healthy.Failed != 0 || healthy.LastSuccessAt == nil {
		t.Fatalf("healthy receipt projection = %#v", healthy)
	}

	s.connectorReceiptMu.Lock()
	s.connectorReceiptStarted++
	s.connectorReceiptMu.Unlock()
	s.observeConnectorReceipt(false)
	degraded := s.connectorReceiptHealthSnapshot()
	if degraded.State != "degraded" || degraded.Durable || degraded.Started != 2 || degraded.Completed != 2 || degraded.Succeeded != 1 || degraded.Failed != 1 || degraded.LastFailureAt == nil {
		t.Fatalf("degraded receipt projection = %#v", degraded)
	}
}
