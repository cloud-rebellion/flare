package api

import "testing"

func TestOverviewConnectorOnlyProjectIsReporting(t *testing.T) {
	var intake overviewIntake
	intake.add("connectors", 2, 1, 0, 100)
	if intake.Accepted24h != 2 || !intake.Connectors || intake.LastAcceptedUnix != 100 {
		t.Fatalf("accepted provider calls must count as runtime reporting: %+v", intake)
	}
	intake.add("symbols", 50, 3, 4, 200)
	if intake.Accepted24h != 2 || intake.LastAcceptedUnix != 100 {
		t.Fatalf("artifact uploads must not inflate runtime reporting: %+v", intake)
	}
	if intake.Rejected24h != 4 || intake.Dropped24h != 4 {
		t.Fatalf("artifact rejection and drop accounting must remain visible: %+v", intake)
	}
}

func TestOverviewRejectedIntakeDoesNotProveCoverage(t *testing.T) {
	var intake overviewIntake
	intake.add("connectors", 0, 5, 1, 200)
	if intake.Accepted24h != 0 || intake.Connectors || intake.LastAcceptedUnix != 0 {
		t.Fatalf("rejected-only calls must not prove accepted coverage: %+v", intake)
	}
	if intake.Rejected24h != 5 || intake.Dropped24h != 1 {
		t.Fatalf("rejected-only accounting was lost: %+v", intake)
	}
}
