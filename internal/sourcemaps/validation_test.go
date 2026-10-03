package sourcemaps

import "testing"

func TestValidateSourceMapStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		map_ string
		want ParseStatus
	}{
		{
			name: "valid v3 map",
			map_: `{"version":3,"sources":["app.ts"],"names":[],"mappings":"AAAA"}`,
			want: ParseStatusValid,
		},
		{
			name: "valid indexed map",
			map_: `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sources":["app.ts"],"names":[],"mappings":"AAAA"}}]}`,
			want: ParseStatusValid,
		},
		{
			name: "malformed json",
			map_: `{"version":3,"mappings":`,
			want: ParseStatusInvalidJSON,
		},
		{
			name: "empty mappings",
			map_: `{"version":3,"sources":["app.ts"],"names":[],"mappings":""}`,
			want: ParseStatusInvalidMappings,
		},
		{
			name: "invalid mappings",
			map_: `{"version":3,"sources":["app.ts"],"names":[],"mappings":"!!!"}`,
			want: ParseStatusInvalidMappings,
		},
		{
			name: "unsupported version",
			map_: `{"version":2,"sources":["app.ts"],"names":[],"mappings":"AAAA"}`,
			want: ParseStatusUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate([]byte(tc.map_)); got != tc.want {
				t.Fatalf("Validate() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateNeverPanicsOnMalformedIndexedSection(t *testing.T) {
	map_ := `{"version":3,"sections":[{"offset":{"line":0,"column":0}}]}`
	if got := Validate([]byte(map_)); got != ParseStatusInvalidMappings {
		t.Fatalf("Validate() = %q, want %q", got, ParseStatusInvalidMappings)
	}
}
