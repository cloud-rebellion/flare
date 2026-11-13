package scan

import (
	"strings"
	"testing"
)

// A pair gets the same outcome as the same text in one string, at least.
func TestPairMatchesTheOneStringOutcome(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := []struct {
		key, value string
		control    string // Text(key=value), empty when the pair is deliberately stricter
		wantKey    string
		wantValue  string
	}{
		{"password", "hunter2hunter2", "password=[secret]", "password", "[secret]"},
		{"x/password", "hunter2hunter2", "x/password=[secret]", "x/password", "[secret]"},
		{"password:", "hunter2hunter2", "", "password:", "[secret]"},
		{"pass​word", "hunter2hunter2", "password=[secret]", "password", "[secret]"},
		{"tokens_used", "1523400", "tokens_used=1523400", "tokens_used", "1523400"},
		{"route", "/a/b", "route=/a/b", "route", "/a/b"},
		{"password", "hunter2 hunter2", "", "password", "[secret]"},
		{"authorization", "abcd1234abcd", "", "authorization", "[secret]"},
		{"cookie", "sid=abcd1234", "", "cookie", "[secret]"},
		{"set-cookie", "sid=abcd1234", "", "set-cookie", "[secret]"},
		{"release", sha, "", "release", sha},
		{"note", sha, "note=[hash]", "note", "[hash]"},
		{"a@x.se", "1", "[email]=1", "[email]", "1"},
		{"password", "", "", "password", ""},
	}
	for _, tt := range cases {
		key, value := Pair(tt.key, tt.value)
		if key != tt.wantKey || value != tt.wantValue {
			t.Errorf("Pair(%q, %q) = %q, %q, want %q, %q", tt.key, tt.value, key, value, tt.wantKey, tt.wantValue)
		}
		if tt.control != "" {
			if got := Text(tt.key + "=" + tt.value); got != tt.control {
				t.Errorf("control Text(%q=%q) = %q, want %q", tt.key, tt.value, got, tt.control)
			}
			if got := key + "=" + value; got != tt.control {
				t.Errorf("Pair(%q, %q) joined = %q, one string gives %q", tt.key, tt.value, got, tt.control)
			}
		}
	}
}

func TestPairsDedupeAfterScrubbingAndAreIdempotent(t *testing.T) {
	got := Pairs([]KeyValue{{"[email]#2", "literal"}, {"a@x.se", "1"}, {"b@x.se", "2"}, {"c@x.se", "3"}})
	want := []KeyValue{{"[email]#2", "literal"}, {"[email]", "1"}, {"[email]#3", "2"}, {"[email]#4", "3"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	again := Pairs(append([]KeyValue(nil), got...))
	for i := range again {
		if again[i] != got[i] {
			t.Fatalf("second pass changed the output: %+v then %+v", got, again)
		}
	}
}

// The structural walker must use the pair rule, or a Struct pass after Pairs
// would rewrite a preserved release SHA and drop the key context.
func TestStructScrubsPairSlicesWithKeyContext(t *testing.T) {
	sha := strings.Repeat("a", 40)
	v := []struct {
		Tags []KeyValue `json:"tags"`
	}{{Tags: []KeyValue{{"password", "hunter2hunter2"}, {"release", sha}, {"a@x.se", "1"}, {"b@x.se", "2"}}}}
	Struct(v)
	want := []KeyValue{{"password", "[secret]"}, {"release", sha}, {"[email]", "1"}, {"[email]#2", "2"}}
	for i := range want {
		if v[0].Tags[i] != want[i] {
			t.Fatalf("got %+v, want %+v", v[0].Tags, want)
		}
	}
}
