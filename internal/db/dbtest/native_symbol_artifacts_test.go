package dbtest

import (
	"context"
	"testing"

	"github.com/bright-interaction/flare/internal/db/generated"
	"github.com/bright-interaction/flare/internal/id"
)

// TestNativeSymbolArtifactsStayTenantScoped exercises the real generated
// queries when TEST_DATABASE_URL is available. The artifact bytes are accepted
// by the write query, while list/delete/exists always carry both tenant keys.
func TestNativeSymbolArtifactsStayTenantScoped(t *testing.T) {
	ctx := context.Background()
	_, q := newPool(t)
	orgA, projectA := seedProject(t, ctx, q)
	orgB, projectB := seedProject(t, ctx, q)

	row, err := q.UpsertNativeSymbolArtifact(ctx, generated.UpsertNativeSymbolArtifactParams{
		ID: id.New(), ProjectID: projectA, OrgID: orgA,
		Release: "mobile-1", Platform: "flutter", Format: "dart_symbols",
		Name: "app.android-arm64.symbols", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Size: 7, Content: []byte("symbols"), ValidationStatus: "unsupported",
	})
	if err != nil {
		t.Fatalf("insert native symbol artifact: %v", err)
	}
	if row.ProjectID != projectA || row.OrgID != orgA || row.Size != 7 || row.ValidationStatus != "unsupported" {
		t.Fatalf("stored metadata = %#v, want project/org/size scoped to seed", row)
	}

	listed, err := q.ListNativeSymbolArtifactsByProject(ctx, generated.ListNativeSymbolArtifactsByProjectParams{
		ProjectID: projectA, OrgID: orgA, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list project A artifacts: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != row.ID || listed[0].Digest != row.Digest {
		t.Fatalf("project A list = %#v, want one metadata row", listed)
	}

	other, err := q.ListNativeSymbolArtifactsByProject(ctx, generated.ListNativeSymbolArtifactsByProjectParams{
		ProjectID: projectB, OrgID: orgB, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list project B artifacts: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("project B saw project A artifacts: %#v", other)
	}

	metadata, err := q.ListNativeSymbolArtifactMetadataForRelease(ctx, generated.ListNativeSymbolArtifactMetadataForReleaseParams{
		ProjectID: projectA, OrgID: orgA, Release: "mobile-1", RowLimit: 10,
	})
	if err != nil {
		t.Fatalf("metadata release read: %v", err)
	}
	if len(metadata) != 1 || metadata[0].ID != row.ID || metadata[0].Size != row.Size {
		t.Fatalf("release metadata = %#v, want one bounded metadata row", metadata)
	}
	crossRelease, err := q.ListNativeSymbolArtifactMetadataForRelease(ctx, generated.ListNativeSymbolArtifactMetadataForReleaseParams{
		ProjectID: projectA, OrgID: orgB, Release: "mobile-1", RowLimit: 10,
	})
	if err != nil {
		t.Fatalf("cross-tenant metadata read: %v", err)
	}
	if len(crossRelease) != 0 {
		t.Fatalf("cross-tenant release metadata leaked: %#v", crossRelease)
	}
	content, err := q.GetNativeSymbolArtifactContentForValidation(ctx, generated.GetNativeSymbolArtifactContentForValidationParams{
		ID: row.ID, ProjectID: projectA, OrgID: orgA, MaxBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("tenant validation read: %v", err)
	}
	if string(content) != "symbols" {
		t.Fatalf("validation content = %q, want stored bytes only inside the server", content)
	}
	crossContent, err := q.GetNativeSymbolArtifactContentForValidation(ctx, generated.GetNativeSymbolArtifactContentForValidationParams{
		ID: row.ID, ProjectID: projectA, OrgID: orgB, MaxBytes: 8 << 20,
	})
	if err == nil || crossContent != nil {
		t.Fatalf("cross-tenant validation content = %q, err=%v", crossContent, err)
	}

	exists, err := q.NativeSymbolArtifactExists(ctx, generated.NativeSymbolArtifactExistsParams{
		ProjectID: projectA, OrgID: orgB, Release: "mobile-1", Platform: "flutter",
		Format: "dart_symbols", Name: "app.android-arm64.symbols",
	})
	if err != nil {
		t.Fatalf("cross-tenant exists: %v", err)
	}
	if exists {
		t.Fatal("cross-tenant org key found a native symbol artifact")
	}

	if deleted, err := q.DeleteNativeSymbolArtifact(ctx, generated.DeleteNativeSymbolArtifactParams{
		ID: row.ID, ProjectID: projectA, OrgID: orgB,
	}); err != nil {
		t.Fatalf("cross-tenant delete: %v", err)
	} else if deleted != 0 {
		t.Fatalf("cross-tenant delete removed %d rows", deleted)
	}
	if deleted, err := q.DeleteNativeSymbolArtifact(ctx, generated.DeleteNativeSymbolArtifactParams{
		ID: row.ID, ProjectID: projectA, OrgID: orgA,
	}); err != nil {
		t.Fatalf("tenant delete: %v", err)
	} else if deleted != 1 {
		t.Fatalf("tenant delete removed %d rows, want 1", deleted)
	}
}
