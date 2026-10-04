package sourcemaps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// A minimal valid source map: generated position (line 1, col 0) maps to
// original orig.js line 1 col 0 ("AAAA"), with inline sourcesContent.
const miniMap = `{"version":3,"sources":["orig.js"],"sourcesContent":["const x = 1;\nthrow new Error('boom');\nconst y = 2;"],"names":[],"mappings":"AAAA"}`

func TestSymbolicateResolvesFrame(t *testing.T) {
	raw := json.RawMessage(`{"frames":[{"filename":"app.min.js","function":"c","lineno":1,"colno":0}]}`)
	out := NewResolver().Symbolicate(raw, map[string]string{"app.min.js": miniMap})

	var st struct {
		Frames []Frame `json:"frames"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f := st.Frames[0]
	if !f.Symbolicated {
		t.Fatalf("frame not symbolicated: %+v", f)
	}
	if f.Filename != "orig.js" {
		t.Errorf("filename = %q, want orig.js", f.Filename)
	}
	if f.Lineno != 1 {
		t.Errorf("lineno = %d, want 1", f.Lineno)
	}
	if f.ContextLine != "const x = 1;" {
		t.Errorf("context_line = %q, want %q", f.ContextLine, "const x = 1;")
	}
}

func TestSymbolicateNoMapIsNoop(t *testing.T) {
	raw := json.RawMessage(`{"frames":[{"filename":"app.min.js","lineno":1,"colno":5}]}`)
	out := NewResolver().Symbolicate(raw, nil)
	if string(out) != string(raw) {
		t.Errorf("expected raw passthrough with no maps, got %s", out)
	}
}

func TestSymbolicateUnmatchedFilePassesThrough(t *testing.T) {
	raw := json.RawMessage(`{"frames":[{"filename":"other.js","lineno":1,"colno":0}]}`)
	out := NewResolver().Symbolicate(raw, map[string]string{"app.min.js": miniMap})
	if string(out) != string(raw) {
		t.Errorf("unmatched file should pass through unchanged, got %s", out)
	}
}

func TestSymbolicateOpaqueBrowserAssetName(t *testing.T) {
	// The browser hashes origin + pathname, leaving query strings out. The
	// authenticated CI uploader uses this name for the same release's map.
	asset := "https://app.example/_app/immutable/chunks/app.BhOPzaDi.js"
	name := fmt.Sprintf("flare-asset-sha256:%x", sha256.Sum256([]byte(asset)))
	raw, err := json.Marshal(struct {
		Frames []Frame `json:"frames"`
	}{Frames: []Frame{{Filename: name, Function: "view", Lineno: 1, Colno: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	out := NewResolver().Symbolicate(raw, map[string]string{name: miniMap})
	var trace struct {
		Frames []Frame `json:"frames"`
	}
	if err := json.Unmarshal(out, &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.Frames) != 1 || !trace.Frames[0].Symbolicated || trace.Frames[0].Filename != "orig.js" {
		t.Fatalf("opaque browser frame did not resolve: %+v", trace.Frames)
	}
}

func TestOpaqueBrowserColumnUsesOneBasedStackPosition(t *testing.T) {
	// Generated columns 0 and 1 map to different source lines. Looking up a
	// browser stack's column 1 as if it were zero-based silently picks line 3.
	const adjacentMap = `{"version":3,"sources":["orig.js"],"sourcesContent":["first\nskip\nsecond"],"names":[],"mappings":"AAAA,CAEA"}`
	raw := json.RawMessage(`{"frames":[{"filename":"flare-asset-sha256:abcdef","lineno":1,"colno":1}]}`)
	out := NewResolver().Symbolicate(raw, map[string]string{"flare-asset-sha256:abcdef": adjacentMap})
	var trace struct {
		Frames []Frame `json:"frames"`
	}
	if err := json.Unmarshal(out, &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.Frames) != 1 || !trace.Frames[0].Symbolicated || trace.Frames[0].Lineno != 1 {
		t.Fatalf("browser column 1 resolved wrong source segment: %+v", trace.Frames)
	}
}
