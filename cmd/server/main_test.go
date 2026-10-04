package main

import (
	"strings"
	"testing"
)

func TestReleaseIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, compiled, declared, want string
	}{
		{name: "helm image agrees", compiled: "v0.4.0", declared: "v0.4.0", want: "v0.4.0"},
		{name: "development declaration keeps compiled identity", compiled: "dev", declared: "development", want: "dev"},
		{name: "development build agrees", compiled: "development", declared: "development", want: "development"},
		{name: "unset declaration keeps compiled identity", compiled: "v0.4.0", want: "v0.4.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := releaseIdentity(tc.compiled, tc.declared)
			if err != nil || got != tc.want {
				t.Fatalf("releaseIdentity(%q, %q) = %q, %v; want %q", tc.compiled, tc.declared, got, err, tc.want)
			}
		})
	}

	if _, err := releaseIdentity("v0.4.0", "v0.3.0"); err == nil {
		t.Fatal("releaseIdentity must reject a declared image release that differs from the binary")
	}
	if _, err := releaseIdentity("v0.4.0", "development"); err == nil {
		t.Fatal("releaseIdentity must reject development identity on a release binary")
	}
}

func TestSourceCommitIdentityAndExactRelease(t *testing.T) {
	sha := strings.Repeat("a", 40)
	got, err := sourceCommitIdentity(sha, sha)
	if err != nil || got != sha {
		t.Fatalf("sourceCommitIdentity = %q, %v; want %q", got, err, sha)
	}
	if got := exactRelease("v0.4.0", sha); got != "v0.4.0+g"+sha {
		t.Fatalf("exactRelease = %q", got)
	}
	if got := exactRelease("dev", ""); got != "dev" {
		t.Fatalf("unattributed local release = %q", got)
	}
	if _, err := sourceCommitIdentity("", sha); err == nil {
		t.Fatal("runtime commit declaration must not invent an unstamped revision")
	}
	if _, err := sourceCommitIdentity(sha, ""); err == nil {
		t.Fatal("stamped binary must have a matching runtime commit declaration")
	}
	if _, err := sourceCommitIdentity(sha, strings.Repeat("b", 40)); err == nil {
		t.Fatal("runtime commit declaration must match the binary")
	}
	for _, invalid := range []string{"abc123", strings.Repeat("G", 40), strings.Repeat("a", 39) + "z"} {
		if _, err := sourceCommitIdentity(invalid, ""); err == nil {
			t.Fatalf("accepted invalid Git object ID %q", invalid)
		}
	}
}
