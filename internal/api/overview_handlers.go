package api

import (
	"net/http"
	"sort"
	"time"
)

const overviewBuckets = 24 // one per hour, last 24h

type overviewHourBucket struct {
	Hour  string `json:"hour"` // RFC3339, top of the hour, UTC
	Count int64  `json:"count"`
}

type overviewIssue struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Level       string `json:"level"`
	EventCount  int64  `json:"event_count"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
}

type overviewProject struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Slug         string         `json:"slug"`
	Events24h    int64          `json:"events_24h"`
	Unresolved   int64          `json:"unresolved"`
	Status       string         `json:"status"`         // healthy | alert | spike | silent
	LastSeenUnix int64          `json:"last_seen_unix"` // newest signal of any level; 0 = no pulse in 7d
	Volume       []int64        `json:"volume"`         // overviewBuckets hourly error counts
	Intake       overviewIntake `json:"intake"`
}

// Intake proves completed receipt acceptance, not instrumentation completeness
// or application health. Sentry envelopes can contain errors or transactions.
type overviewIntake struct {
	Accepted24h      int64 `json:"accepted_24h"`
	Rejected24h      int64 `json:"rejected_24h"`
	Dropped24h       int64 `json:"dropped_24h"`
	LastAcceptedUnix int64 `json:"last_accepted_unix"`
	Sentry           bool  `json:"sentry"`
	Traces           bool  `json:"traces"`
	Logs             bool  `json:"logs"`
	Metrics          bool  `json:"metrics"`
	Business         bool  `json:"business"`
}

type overviewResponse struct {
	Events24h            int64                `json:"events_24h"`
	Unresolved           int64                `json:"unresolved"`
	NewToday             int64                `json:"new_today"`
	Volume               []overviewHourBucket `json:"volume"`
	TopIssues            []overviewIssue      `json:"top_issues"`
	Projects             []overviewProject    `json:"projects"`
	ReportingProjects24h int64                `json:"reporting_projects_24h"`
	Rejected24h          int64                `json:"rejected_24h"`
	Dropped24h           int64                `json:"dropped_24h"`
	// AlertingBlind is true when the org has enabled alert rules but NO enabled
	// notification channel: alerts would fire into the void and reach no human.
	// This is the guardrail against the estate's "nothing could page a human"
	// failure mode; the dashboard surfaces it as a banner.
	AlertingBlind bool `json:"alerting_blind"`
}

// handleOverview returns the org-wide dashboard: 24h event volume, headline
// counts, the top unresolved issues, and a per-project health card with an
// hourly sparkline. Everything is org-scoped; a member API key or a session
// can read it.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	org := orgIDFrom(ctx)
	now := time.Now()

	// Bucket 0 is the oldest hour, bucket overviewBuckets-1 is the current
	// (partial) hour. Everything aligns to the top of the hour in UTC.
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(overviewBuckets-1) * time.Hour)
	bucketIndex := func(t time.Time) int {
		return int(t.UTC().Truncate(time.Hour).Sub(base) / time.Hour)
	}

	events24h, err := s.q.OverviewEventCount24h(ctx, org)
	if err != nil {
		slogError(w, "overview event count", err)
		return
	}
	unresolved, err := s.q.OverviewUnresolvedCount(ctx, org)
	if err != nil {
		slogError(w, "overview unresolved count", err)
		return
	}
	newToday, err := s.q.OverviewNewIssuesToday(ctx, org)
	if err != nil {
		slogError(w, "overview new today", err)
		return
	}

	// Org-wide hourly volume, zero-filled.
	volume := make([]overviewHourBucket, overviewBuckets)
	for i := range volume {
		volume[i] = overviewHourBucket{Hour: base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)}
	}
	volRows, err := s.q.OverviewEventVolumeByHour(ctx, org)
	if err != nil {
		slogError(w, "overview volume", err)
		return
	}
	for _, row := range volRows {
		if !row.Hour.Valid {
			continue
		}
		if i := bucketIndex(row.Hour.Time); i >= 0 && i < overviewBuckets {
			volume[i].Count = row.Count
		}
	}

	topRows, err := s.q.OverviewTopIssues(ctx, org)
	if err != nil {
		slogError(w, "overview top issues", err)
		return
	}
	top := make([]overviewIssue, 0, len(topRows))
	for _, row := range topRows {
		top = append(top, overviewIssue{
			ID: row.ID, Title: row.Title, Level: row.Level,
			EventCount: row.EventCount, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
		})
	}

	// Per-project: hourly volume + unresolved count, keyed by project id.
	projVol := map[string][]int64{}
	projTotal := map[string]int64{}
	pvRows, err := s.q.OverviewProjectVolumeByHour(ctx, org)
	if err != nil {
		slogError(w, "overview project volume", err)
		return
	}
	for _, row := range pvRows {
		if !row.Hour.Valid {
			continue
		}
		i := bucketIndex(row.Hour.Time)
		if i < 0 || i >= overviewBuckets {
			continue
		}
		b := projVol[row.ProjectID]
		if b == nil {
			b = make([]int64, overviewBuckets)
			projVol[row.ProjectID] = b
		}
		b[i] = row.Count
		projTotal[row.ProjectID] += row.Count
	}
	projUnres := map[string]int64{}
	puRows, err := s.q.OverviewProjectUnresolved(ctx, org)
	if err != nil {
		slogError(w, "overview project unresolved", err)
		return
	}
	for _, row := range puRows {
		projUnres[row.ProjectID] = row.Count
	}

	// Liveness pulse: newest signal of ANY level (heartbeats included), so a
	// service that is up but not erroring still reads as alive rather than dead.
	projLastSeen := map[string]time.Time{}
	lsRows, err := s.q.OverviewProjectLastSeen(ctx, org)
	if err != nil {
		slogError(w, "overview project last seen", err)
		return
	}
	for _, row := range lsRows {
		if row.LastSeen.Valid {
			projLastSeen[row.ProjectID] = row.LastSeen.Time
		}
	}

	intakeByProject := map[string]overviewIntake{}
	intakeRows, err := s.q.OverviewIngestCoverage24h(ctx, org)
	if err != nil {
		slogError(w, "overview ingest coverage", err)
		return
	}
	for _, row := range intakeRows {
		v := intakeByProject[row.ProjectID]
		v.Rejected24h += row.RejectedCount
		v.Dropped24h += row.DroppedCount
		// Symbol uploads and connector setup are useful receipts, but they do
		// not prove that an application is reporting runtime telemetry.
		runtime := false
		switch row.Pillar {
		case "event", "envelope", "spans", "logs", "metrics", "business":
			runtime = true
		}
		if runtime {
			v.Accepted24h += row.AcceptedCount
		}
		if runtime && row.AcceptedCount > 0 {
			if row.LastAcceptedAt.Valid && row.LastAcceptedAt.Time.Unix() > v.LastAcceptedUnix {
				v.LastAcceptedUnix = row.LastAcceptedAt.Time.Unix()
			}
			switch row.Pillar {
			case "event", "envelope":
				v.Sentry = true
			case "spans":
				v.Traces = true
			case "logs":
				v.Logs = true
			case "metrics":
				v.Metrics = true
			case "business":
				v.Business = true
			}
		}
		intakeByProject[row.ProjectID] = v
	}

	projects, err := s.q.ListProjectsByOrg(ctx, org)
	if err != nil {
		slogError(w, "overview projects", err)
		return
	}
	cards := make([]overviewProject, 0, len(projects))
	var reportingProjects, rejected24h, dropped24h int64
	for _, p := range projects {
		intake := intakeByProject[p.ID]
		if intake.Accepted24h > 0 {
			reportingProjects++
		}
		rejected24h += intake.Rejected24h
		dropped24h += intake.Dropped24h
		vol := projVol[p.ID]
		if vol == nil {
			vol = make([]int64, overviewBuckets)
		}
		total := projTotal[p.ID]
		lastSeen := projLastSeen[p.ID]
		alive := !lastSeen.IsZero() && now.Sub(lastSeen) <= livenessWindow
		var lastSeenUnix int64
		if !lastSeen.IsZero() {
			lastSeenUnix = lastSeen.Unix()
		}
		cards = append(cards, overviewProject{
			ID: p.ID, Name: p.Name, Slug: p.Slug,
			Events24h:    total,
			Unresolved:   projUnres[p.ID],
			Status:       projectHealth(alive, vol, total),
			LastSeenUnix: lastSeenUnix,
			Volume:       vol,
			Intake:       intake,
		})
	}
	// Order by health (attention first, silent last), then busiest, then name.
	// The dashboard groups by status too, but this keeps the raw list sensible.
	sort.SliceStable(cards, func(i, j int) bool {
		if ri, rj := healthRank(cards[i].Status), healthRank(cards[j].Status); ri != rj {
			return ri < rj
		}
		if cards[i].Events24h != cards[j].Events24h {
			return cards[i].Events24h > cards[j].Events24h
		}
		return cards[i].Name < cards[j].Name
	})

	// Alerting-blind guardrail: rules exist but no channel can carry them.
	enabledChannels, err := s.q.CountEnabledNotificationChannelsByOrg(ctx, org)
	if err != nil {
		slogError(w, "overview channel count", err)
		return
	}
	enabledRules, err := s.q.CountEnabledAlertRulesByOrg(ctx, org)
	if err != nil {
		slogError(w, "overview rule count", err)
		return
	}

	writeJSON(w, http.StatusOK, overviewResponse{
		Events24h: events24h, Unresolved: unresolved, NewToday: newToday,
		Volume: volume, TopIssues: top, Projects: cards,
		ReportingProjects24h: reportingProjects,
		Rejected24h:          rejected24h, Dropped24h: dropped24h,
		AlertingBlind: enabledRules > 0 && enabledChannels == 0,
	})
}

// livenessWindow is how long a project can go with no signal of any level
// (heartbeats included) before it counts as silent on the dashboard. The estate
// heartbeats every ~2-3 minutes, so 15 minutes clears six missed beats before
// flagging: fast enough to catch a real death, slack enough to never flicker on
// a healthy service. A project whose heartbeat cadence is genuinely slower than
// this would need a longer window (or an explicit check-in monitor).
const livenessWindow = 15 * time.Minute

// projectHealth classifies a project card from its liveness pulse and error
// activity. States (most to least urgent):
//
//	spike   - alive AND the current hour is a meaningful error surge (>=5 events
//	          and >=3x the trailing hourly average).
//	alert   - alive AND producing errors in the window, but not spiking.
//	healthy - alive AND no errors in the window. The reassuring default for a
//	          service that is simply up and clean (most of the estate).
//	silent  - no pulse within livenessWindow. This wins over every error state:
//	          a process that is not reporting at all cannot be meaningfully
//	          "erroring", and "is it even alive?" is the headline the operator
//	          needs. Covers dead, retired, and never-wired projects alike.
//
// alive is whether the newest signal of any level landed within livenessWindow.
// errTotal is the 24h error count (info/debug already excluded upstream); vol is
// the zero-filled hourly error series with the current (partial) hour last.
func projectHealth(alive bool, vol []int64, errTotal int64) string {
	if !alive {
		return "silent"
	}
	if errTotal == 0 {
		return "healthy"
	}
	if len(vol) >= 2 {
		last := vol[len(vol)-1]
		var prior int64
		for _, c := range vol[:len(vol)-1] {
			prior += c
		}
		avg := float64(prior) / float64(len(vol)-1)
		if last >= 5 && float64(last) >= 3*avg {
			return "spike"
		}
	}
	return "alert"
}

// healthRank orders statuses from most to least urgent for the flat project list.
func healthRank(status string) int {
	switch status {
	case "spike":
		return 0
	case "alert":
		return 1
	case "healthy":
		return 2
	case "silent":
		return 3
	default:
		return 4
	}
}
