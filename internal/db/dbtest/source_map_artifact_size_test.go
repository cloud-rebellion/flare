package dbtest

import (
	"context"
	"testing"
	"unicode/utf8"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// Source-map quotas and response sizes count UTF-8 bytes, including when the
// map embeds Unicode source names or source content. This needs real Postgres:
// character length and octet length differ for TEXT.
func TestSourceMapArtifactSizesUseStoredBytes(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	org, project := seedProject(t, ctx, q)
	const content = `{"version":3,"sources":["café.ts"],"sourcesContent":["const snowman = '☃';"],"names":[],"mappings":"AAAA"}`
	wantBytes := len(content)
	if wantBytes == utf8.RuneCountInString(content) {
		t.Fatal("Unicode fixture does not distinguish byte and character lengths")
	}

	inserted, err := q.UpsertSourceMap(ctx, generated.UpsertSourceMapParams{
		ID: id.New(), ProjectID: project, OrgID: org, Release: "web-1",
		Name: "app.min.js", Content: content, ParseStatus: "valid",
	})
	if err != nil {
		t.Fatalf("insert source map: %v", err)
	}
	if inserted.Size != int32(wantBytes) {
		t.Fatalf("upload size = %v, want %d UTF-8 bytes", inserted.Size, wantBytes)
	}

	listed, err := q.ListSourceMapsByProject(ctx, generated.ListSourceMapsByProjectParams{
		ProjectID: project, OrgID: org,
	})
	if err != nil {
		t.Fatalf("list source maps: %v", err)
	}
	if len(listed) != 1 || listed[0].Size != int32(wantBytes) {
		t.Fatalf("listed size = %#v, want %d UTF-8 bytes", listed, wantBytes)
	}

	usage, err := q.CountSourceMapsByProject(ctx, generated.CountSourceMapsByProjectParams{
		ProjectID: project, OrgID: org,
	})
	if err != nil {
		t.Fatalf("count source maps: %v", err)
	}
	if usage.Artifacts != 1 || usage.Bytes != int64(wantBytes) {
		t.Fatalf("quota usage = %+v, want 1 artifact and %d UTF-8 bytes", usage, wantBytes)
	}

	for _, tc := range []struct {
		maxBytes int64
		wantMaps int
	}{
		{maxBytes: int64(wantBytes - 1), wantMaps: 0},
		{maxBytes: int64(wantBytes), wantMaps: 1},
	} {
		maps, err := q.GetSourceMapsForRelease(ctx, generated.GetSourceMapsForReleaseParams{
			ProjectID: project, OrgID: org, Release: "web-1", MaxBytes: tc.maxBytes,
		})
		if err != nil {
			t.Fatalf("read source maps with %d-byte budget: %v", tc.maxBytes, err)
		}
		if len(maps) != tc.wantMaps {
			t.Fatalf("%d-byte budget returned %d maps, want %d", tc.maxBytes, len(maps), tc.wantMaps)
		}
	}
}
