package connectorcheck

import (
	"context"
	"testing"
)

type fakeProbe struct{}

func (fakeProbe) Check(context.Context, string) (Result, error) { return Result{Outcome: "ok"}, nil }

func TestRegistryCopiesOnlyBoundedAdapters(t *testing.T) {
	probe := fakeProbe{}
	input := map[string]Adapter{
		"hubspot": {Operation: "account_details", Probe: probe},
		"":        {Operation: "invalid", Probe: probe},
		"slack":   {Operation: "", Probe: probe},
	}
	registry := NewRegistry(input)
	input["hubspot"] = Adapter{}
	if got, ok := registry.Lookup(" hubspot "); !ok || got.Operation != "account_details" {
		t.Fatalf("hubspot adapter = %#v/%v", got, ok)
	}
	if _, ok := registry.Lookup("slack"); ok {
		t.Fatal("adapter without an operation was registered")
	}
	if _, ok := registry.Lookup("unknown"); ok {
		t.Fatal("unknown provider was registered")
	}
}
