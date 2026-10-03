package api

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bright-interaction/flare/internal/native_symbols"
)

func TestNormalizeNativeSymbolUploadBoundsAndFormats(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("symbols"))
	for _, tc := range []struct {
		name     string
		request  nativeSymbolUploadRequest
		wantErr  string
		wantSize int
	}{
		{
			name: "flutter dart symbols",
			request: nativeSymbolUploadRequest{
				Release: "mobile@1.8.0+42", Platform: "flutter", Format: "dart_symbols",
				Name: "app.android-arm64.symbols", Content: payload,
			}, wantSize: len("symbols"),
		},
		{
			name: "ios dsym binary",
			request: nativeSymbolUploadRequest{
				Release: "ios-42", Platform: "ios", Format: "dsym",
				Name: "Runner.dSYM.zip", Content: payload,
			}, wantSize: len("symbols"),
		},
		{
			name: "android proguard",
			request: nativeSymbolUploadRequest{
				Release: "android-42", Platform: "android", Format: "proguard",
				Name: "mapping.txt", Content: payload,
			}, wantSize: len("symbols"),
		},
		{
			name: "format platform mismatch",
			request: nativeSymbolUploadRequest{
				Release: "android-42", Platform: "android", Format: "dsym",
				Name: "Runner.dSYM.zip", Content: payload,
			}, wantErr: "format dsym requires ios",
		},
		{
			name: "bad name path",
			request: nativeSymbolUploadRequest{
				Release: "ios-42", Platform: "ios", Format: "dsym",
				Name: "../Runner.dSYM.zip", Content: payload,
			}, wantErr: "name is required",
		},
		{
			name: "bad base64",
			request: nativeSymbolUploadRequest{
				Release: "ios-42", Platform: "ios", Format: "dsym",
				Name: "Runner.dSYM.zip", Content: "!not-base64!",
			}, wantErr: "not valid base64",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, raw, err := normalizeNativeSymbolUpload(tc.request)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got.Release != tc.request.Release || got.Platform != tc.request.Platform || got.Format != tc.request.Format {
				t.Fatalf("normalized metadata = %#v, want %#v", got, tc.request)
			}
			if len(raw) != tc.wantSize {
				t.Fatalf("decoded size = %d, want %d", len(raw), tc.wantSize)
			}
		})
	}
}

func TestNativeSymbolUploadRejectsOversizedArtifact(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, maxNativeSymbolBytes+1))
	_, _, err := normalizeNativeSymbolUpload(nativeSymbolUploadRequest{
		Release: "flutter-1", Platform: "flutter", Format: "dart_symbols",
		Name: "app.symbols", Content: encoded,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the supported size") {
		t.Fatalf("error = %v, want oversized artifact rejection", err)
	}
}

func TestNativeSymbolValidationStatusIsExplicitAndFormatScoped(t *testing.T) {
	if got := nativeSymbolValidationStatus("flutter", "dart_symbols", nil); got != "invalid" {
		t.Fatalf("empty Flutter symbols status = %q, want invalid", got)
	}
	if got := nativeSymbolValidationStatus("flutter", "dart_symbols", []byte("not-a-container")); got != "unsupported" {
		t.Fatalf("unknown Flutter symbols status = %q, want unsupported", got)
	}
	if got := nativeSymbolValidationStatus("android", "proguard", []byte("class a -> b:")); got != "not_checked" {
		t.Fatalf("unresolved Android format status = %q, want not_checked", got)
	}
	body, err := json.Marshal(nativeSymbolHealthResponse{
		nativeSymbolArtifactResponse: nativeSymbolArtifactResponse{ValidationStatus: "recognized"},
		SymbolicationStatus:          string(native_symbols.StatusRetainedUnresolved),
	})
	if err != nil || !strings.Contains(string(body), `"validation_status":"recognized"`) {
		t.Fatalf("health response lost durable validation status: %s (err=%v)", body, err)
	}
}

func TestNativeSymbolRoutesAreAuthenticatedAndAudited(t *testing.T) {
	router, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	body := string(router)
	readStart := strings.Index(body, "r.Use(s.requireAuth)")
	memberStart := strings.Index(body, `s.requireRole("member")`)
	if readStart < 0 || memberStart < 0 || readStart >= memberStart {
		t.Fatal("router auth/read groups changed")
	}
	readBlock := body[readStart:memberStart]
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/symbol-artifacts", s.handleListNativeSymbolArtifacts)`) {
		t.Fatal("native symbol list is not in the authenticated viewer read group")
	}
	if !strings.Contains(readBlock, `r.Get("/projects/{id}/native-symbol-health", s.handleListNativeSymbolHealth)`) {
		t.Fatal("native symbol health is not in the authenticated viewer read group")
	}
	if !strings.Contains(body[memberStart:], `r.Post("/projects/{id}/symbol-artifacts", s.handleUploadNativeSymbolArtifact)`) ||
		!strings.Contains(body[memberStart:], `r.Delete("/projects/{id}/symbol-artifacts/{artifactID}", s.handleDeleteNativeSymbolArtifact)`) {
		t.Fatal("native symbol mutations are not in the member write group")
	}
	handler, err := os.ReadFile("native_symbol_artifacts_handlers.go")
	if err != nil {
		t.Fatalf("read native symbol handler: %v", err)
	}
	h := string(handler)
	for _, want := range []string{"GetProjectByID", "OrgScope: orgIDFrom", "UpsertNativeSymbolArtifact", "DeleteNativeSymbolArtifact", "s.audit("} {
		if !strings.Contains(h, want) {
			t.Fatalf("native symbol handler missing %q", want)
		}
	}
	for _, forbidden := range []string{"json:\"content\"", "Content: row.Content", "writeJSON(w, http.StatusOK, row)"} {
		if strings.Contains(h, forbidden) {
			t.Fatalf("native symbol handler appears to expose raw artifact content via %q", forbidden)
		}
	}
}

func TestNativeSymbolQueriesAreTenantScopedAndMetadataOnly(t *testing.T) {
	src, err := os.ReadFile("../db/queries/native_symbol_artifacts.sql")
	if err != nil {
		t.Fatalf("read native symbol queries: %v", err)
	}
	query := string(src)
	for _, required := range []string{
		"project_id = $1 AND org_id = $2",
		"ListNativeSymbolArtifactMetadataForRelease",
		"release = $3",
		"UpsertNativeSymbolArtifact",
		"digest",
		"size",
		"DeleteNativeSymbolArtifact",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("native symbol query missing %q", required)
		}
	}
	if strings.Contains(query, "SELECT content") || strings.Contains(query, "SELECT *") {
		t.Fatal("native symbol metadata query selects raw artifact content")
	}
}

func TestNativeSymbolReadStatusIsExplicitlyUnresolved(t *testing.T) {
	if got := string(native_symbols.StatusRetainedUnresolved); got != "retained_unresolved" {
		t.Fatalf("native status = %q, want explicit unresolved state", got)
	}
	handler, err := os.ReadFile("native_symbol_artifacts_handlers.go")
	if err != nil {
		t.Fatalf("read native symbol handler: %v", err)
	}
	body := string(handler)
	for _, required := range []string{"SymbolicationStatus", "ValidationStatus", "StatusRetainedUnresolved", "artifact bytes", "frame locations", "row.ValidationStatus"} {
		if !strings.Contains(body, required) {
			t.Fatalf("native health handler missing bounded unresolved contract %q", required)
		}
	}
	if strings.Contains(body, "Content: row.Content") || strings.Contains(body, "json:\"content\"") {
		t.Fatal("native health handler exposes raw artifact content")
	}
}

func TestNativeSymbolValidationQueryIsTenantScopedAndBounded(t *testing.T) {
	src, err := os.ReadFile("../db/queries/native_symbol_validation.sql")
	if err != nil {
		t.Fatalf("read validation query: %v", err)
	}
	query := string(src)
	for _, required := range []string{"id = sqlc.arg(id)", "project_id = sqlc.arg(project_id)", "org_id = sqlc.arg(org_id)", "LEAST(sqlc.arg(max_bytes), 8388608)"} {
		if !strings.Contains(query, required) {
			t.Fatalf("validation query missing %q", required)
		}
	}
	if !strings.Contains(query, "SELECT content") {
		t.Fatal("validation query must read content only inside the bounded server-side validator")
	}
}
