package registrybuilder

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/harness"
)

// AC-REG5 / AC-PI8: Build wires both day-one providers from city config
// descriptors and registers them with the per-provider capability declarations.
func TestBuild_RegistersDayOneProviders(t *testing.T) {
	r, err := Build([]ProviderBlock{
		{ID: "pi-rpc", URL: "https://ff-pipeline.koales.workers.dev"},
		{ID: "cloudflare-sandbox", URL: "https://gascity-cloudflare-control-worker.koales.workers.dev"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := r.Get("pi-rpc"); !ok {
		t.Fatalf("expected pi-rpc registered")
	}
	if _, ok := r.Get("cloudflare-sandbox"); !ok {
		t.Fatalf("expected cloudflare-sandbox registered")
	}
}

// AC-REG4: an explicit incomplete tuple in a provider block fails closed.
func TestBuild_RejectsIncompleteTuple(t *testing.T) {
	_, err := Build([]ProviderBlock{
		{ID: "pi-rpc", URL: "https://ff-pipeline.koales.workers.dev", HarnessSlots: []string{"E"}},
	})
	if !errors.Is(err, harness.ErrIncompleteHarnessTuple) {
		t.Fatalf("expected ErrIncompleteHarnessTuple, got %v", err)
	}
}

// An unknown provider id is rejected: Build only knows the day-one providers
// (AC-PI8: pi-rpc is the first compatibility provider, sandbox the second).
func TestBuild_RejectsUnknownProvider(t *testing.T) {
	_, err := Build([]ProviderBlock{
		{ID: "openshell", URL: "https://example.invalid"},
	})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", err)
	}
}

// pi-rpc requires a URL.
func TestBuild_RejectsMissingURL(t *testing.T) {
	_, err := Build([]ProviderBlock{
		{ID: "pi-rpc"},
	})
	if err == nil {
		t.Fatalf("expected an error for a pi-rpc block with no URL")
	}
}

// cloudflare-sandbox requires a URL.
func TestBuild_RejectsSandboxMissingURL(t *testing.T) {
	_, err := Build([]ProviderBlock{
		{ID: "cloudflare-sandbox"},
	})
	if err == nil {
		t.Fatalf("expected an error for a cloudflare-sandbox block with no URL")
	}
}
