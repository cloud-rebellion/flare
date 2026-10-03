package main

import "testing"

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
