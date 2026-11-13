package retention

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestResolvePrecedence(t *testing.T) {
	env := Policy{HotDays: 30, ColdMode: ColdForever}

	got, src, err := Resolve(nil, env)
	if err != nil || src != SourceEnv || got != env {
		t.Fatalf("nothing stored: got %+v %q %v, want env default", got, src, err)
	}

	stored := Policy{HotDays: 7, ColdMode: ColdOff}
	got, src, err = Resolve(&stored, env)
	if err != nil || src != SourceStored || got != stored {
		t.Fatalf("stored policy must win over env: got %+v %q %v", got, src, err)
	}

	// An invalid stored row is an error, never a silent fallback to env: env
	// may keep LESS than what the admin stored.
	bad := Policy{HotDays: 2, ColdMode: ColdForever}
	if _, _, err := Resolve(&bad, Policy{HotDays: 1, ColdMode: ColdOff}); err == nil {
		t.Fatal("an invalid stored policy must not resolve to the env default")
	}
}

func TestValidateBounds(t *testing.T) {
	for _, tc := range []struct {
		p  Policy
		ok bool
	}{
		{Policy{HotDays: 7, ColdMode: ColdOff}, true},
		{Policy{HotDays: 7, ColdMode: ColdForever}, true},
		{Policy{HotDays: 365, ColdMode: ColdForever}, true},
		{Policy{HotDays: 6, ColdMode: ColdForever}, false},  // below the backfill window
		{Policy{HotDays: 0, ColdMode: ColdForever}, false},  // zero
		{Policy{HotDays: -1, ColdMode: ColdForever}, false}, // negative
		{Policy{HotDays: 366, ColdMode: ColdForever}, false},
		{Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 7}, true}, // cold == hot is allowed
		{Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 3650}, true},
		{Policy{HotDays: 30, ColdMode: ColdDays, ColdDays: 29}, false}, // cold < hot while archiving
		{Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 3651}, false},
		{Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 0}, false},
		{Policy{HotDays: 7, ColdMode: ColdOff, ColdDays: 30}, false},     // days only with mode days
		{Policy{HotDays: 7, ColdMode: ColdForever, ColdDays: 30}, false}, // same
		{Policy{HotDays: 7, ColdMode: "", ColdDays: 0}, false},           // mode is required
		{Policy{HotDays: 7, ColdMode: "delete", ColdDays: 0}, false},
	} {
		if err := tc.p.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", tc.p, err, tc.ok)
		}
	}
}

func TestShouldDropHotBoundary(t *testing.T) {
	now := time.Date(2026, 11, 8, 13, 37, 0, 0, time.UTC)
	for _, tc := range []struct {
		day  string
		hot  int
		drop bool
	}{
		// hot=7 keeps 2026-11-01 .. today; 2026-10-31 is the first dropped day.
		{"2026-11-01", 7, false},
		{"2026-10-31", 7, true},
		{"2026-01-01", 7, true},
		{"2026-11-08", 7, false}, // today
		{"2026-11-09", 7, false}, // future (pre-created)
		{"2026-11-11", 7, false},
		// hotDays that would put the cutoff at or past today still never drops
		// today or the future.
		{"2026-11-08", 0, false},
		{"2026-11-08", -5, false},
		{"2026-11-09", -5, false},
		{"2026-11-07", 0, false}, // clamped to 1: yesterday is the oldest kept day
		{"2026-11-06", 0, true},
	} {
		if got := ShouldDropHot(now, day(tc.day), tc.hot); got != tc.drop {
			t.Errorf("ShouldDropHot(%s, hot=%d) = %v, want %v", tc.day, tc.hot, got, tc.drop)
		}
	}
}

func TestShouldDropHotUsesUTCCalendar(t *testing.T) {
	// 00:30 in Stockholm is still the previous UTC day.
	sthlm := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 9, 0, 30, 0, 0, sthlm) // 2026-11-08T23:30Z
	if ShouldDropHot(now, day("2026-11-01"), 7) {
		t.Fatal("cutoff must be computed on the UTC calendar")
	}
	if !ShouldDropHot(now, day("2026-10-31"), 7) {
		t.Fatal("2026-10-31 is past a 7 day window on 2026-11-08 UTC")
	}
}

func TestShouldPruneCold(t *testing.T) {
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	days := Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 30}
	if ShouldPruneCold(now, day("2026-10-09"), days) {
		t.Error("2026-10-09 is inside a 30 day archive window")
	}
	if !ShouldPruneCold(now, day("2026-10-08"), days) {
		t.Error("2026-10-08 is past a 30 day archive window")
	}
	for _, p := range []Policy{{HotDays: 7, ColdMode: ColdForever}, {HotDays: 7, ColdMode: ColdOff}} {
		if ShouldPruneCold(now, day("2000-01-01"), p) {
			t.Errorf("mode %q must never prune the archive", p.ColdMode)
		}
	}
	// A policy that slipped past validation still cannot prune inside the hot
	// window.
	broken := Policy{HotDays: 7, ColdMode: ColdDays, ColdDays: 1}
	if ShouldPruneCold(now, day("2026-11-02"), broken) {
		t.Error("cold pruning reached inside the hot window")
	}
}

// TestReduces runs the table shared with the dashboard
// (frontend/tests/retention.test.mjs reads the same file), so the server gate
// and the UI confirm cannot drift apart. Every row that leaves data behind in
// the previous mode (off -> days in particular) is a reduction: "off" stops
// new exports but keeps every archive day already on disk.
func TestReduces(t *testing.T) {
	raw, err := os.ReadFile("testdata/reduces_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string
		Cur, Next Policy
		Want      bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d shared cases loaded", len(cases))
	}
	for _, tc := range cases {
		if got := Reduces(tc.Cur, tc.Next); got != tc.Want {
			t.Errorf("%s: Reduces(%s -> %s) = %v, want %v", tc.Name, tc.Cur.Describe(), tc.Next.Describe(), got, tc.Want)
		}
	}
}

func TestParseColdEnv(t *testing.T) {
	for _, v := range []string{"", "  ", "forever", "FOREVER"} {
		if m, d, err := ParseColdEnv(v); err != nil || m != ColdForever || d != 0 {
			t.Errorf("ParseColdEnv(%q) = %q %d %v, want forever", v, m, d, err)
		}
	}
	if m, d, err := ParseColdEnv("90"); err != nil || m != ColdDays || d != 90 {
		t.Errorf("ParseColdEnv(90) = %q %d %v", m, d, err)
	}
	// Turning archiving off is a dashboard decision, never an env typo.
	for _, v := range []string{"0", "off", "-1", "abc", "3651"} {
		if _, _, err := ParseColdEnv(v); err == nil || !strings.Contains(err.Error(), "COLD_RETENTION_DAYS") {
			t.Errorf("ParseColdEnv(%q) accepted, want an error naming the variable", v)
		}
	}
}

func TestConservativeForArchivePrune(t *testing.T) {
	d := func(hot, cold int) Policy { return Policy{HotDays: hot, ColdMode: ColdDays, ColdDays: cold} }
	forever := Policy{HotDays: 30, ColdMode: ColdForever}
	off := Policy{HotDays: 30, ColdMode: ColdOff}
	for _, tc := range []struct {
		name string
		a, b Policy
		want Policy
	}{
		{"forever wins", d(30, 30), forever, forever},
		{"forever wins either side", forever, d(30, 30), forever},
		{"off keeps the archive", d(30, 30), off, off},
		{"longer days", d(30, 30), d(30, 90), d(30, 90)},
		{"longer hot clamps too", d(30, 30), d(60, 60), d(60, 60)},
		{"mixed takes the max of each", d(90, 90), d(30, 365), d(90, 365)},
	} {
		got := ConservativeForArchivePrune(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
		if a, b := ConservativeForArchivePrune(tc.a, tc.b), ConservativeForArchivePrune(tc.b, tc.a); a.ColdMode != b.ColdMode || a.ColdDays != b.ColdDays {
			t.Errorf("%s: not symmetric: %+v vs %+v", tc.name, a, b)
		}
	}
}
