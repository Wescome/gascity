// Package registrybuilder constructs a harness.Registry from city configuration
// provider blocks (IS-GC-RUNTIME-PROVIDER-CONTRACT Open Question 3: the registry
// lives in city configuration). It wires the two day-one providers — pi-rpc and
// cloudflare-sandbox — and enforces the tuple-completeness gate (AC-REG4) and
// canonical capability set (AC-REG5) via the registry.
//
// It is a separate subpackage to avoid an import cycle: the provider packages
// import internal/harness, and this builder imports both provider packages.
package registrybuilder

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/harness"
	harnesscloudflare "github.com/gastownhall/gascity/internal/harness/cloudflare"
	"github.com/gastownhall/gascity/internal/harness/pirpc"
	runtimecloudflare "github.com/gastownhall/gascity/internal/runtime/cloudflare"
)

// ErrUnknownProvider reports a provider id that is not one of the day-one
// providers. Per AC-PI8 only pi-rpc and cloudflare-sandbox are registered for
// live city execution; other families (openshell, codex, claude-code, aider,
// opencode, browser, docker, k8s-job) are out of scope for this IS.
var ErrUnknownProvider = errors.New("unknown_provider")

// ProviderBlock is the city-config descriptor for one [provider.<id>] block. It
// is a plain struct (not the config.ProviderSpec) so this package does not
// depend on internal/config, keeping the harness kernel domain-neutral. The
// caller (cmd/gc) maps config.ProviderSpec → ProviderBlock.
type ProviderBlock struct {
	// ID is the provider id (pi-rpc, cloudflare-sandbox).
	ID string
	// URL is the endpoint the provider calls.
	URL string
	// Token is an optional bearer token.
	Token string
	// Version is the optional provider version reported in runtime_identity.
	Version string
	// HarnessSlots is the declared Harness Tuple coverage. When set it overrides
	// the per-provider default and is checked by the registry (AC-REG4).
	HarnessSlots []string
	// CapabilityKeys is the declared capability key set. When set it overrides
	// the per-provider default and is checked by the registry (AC-REG5).
	CapabilityKeys []string
}

// Build constructs a harness.Registry from the city-config provider blocks. It
// registers each block's provider with the registry, which enforces the
// tuple-completeness gate (AC-REG4) and canonical-key gate (AC-REG5). An unknown
// provider id fails closed with ErrUnknownProvider.
func Build(blocks []ProviderBlock) (*harness.Registry, error) {
	r := harness.NewRegistry()
	for _, block := range blocks {
		if err := registerBlock(r, block); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func registerBlock(r *harness.Registry, block ProviderBlock) error {
	switch block.ID {
	case pirpc.ProviderID:
		provider, err := pirpc.New(pirpc.Config{URL: block.URL, Token: block.Token, Version: block.Version})
		if err != nil {
			return fmt.Errorf("provider %q: %w", block.ID, err)
		}
		return r.Register(block.ID, provider, declarationFor(block, pirpc.CapabilityDeclaration()))

	case harnesscloudflare.ProviderID:
		rt, err := runtimecloudflare.NewProviderWithConfig(runtimecloudflare.Config{
			Endpoint: block.URL,
			Token:    block.Token,
		})
		if err != nil {
			return fmt.Errorf("provider %q: %w", block.ID, err)
		}
		provider, err := harnesscloudflare.New(harnesscloudflare.Config{Runtime: rt, Version: block.Version})
		if err != nil {
			return fmt.Errorf("provider %q: %w", block.ID, err)
		}
		return r.Register(block.ID, provider, declarationFor(block, harnesscloudflare.CapabilityDeclaration()))

	default:
		return fmt.Errorf("%w: %q", ErrUnknownProvider, block.ID)
	}
}

// declarationFor returns the capability declaration to register: the block's
// explicit slots/keys when present, else the provider's canonical default. This
// lets a city.toml assert the tuple (and have it checked) while keeping the
// per-provider default as the source of truth when the block omits it.
func declarationFor(block ProviderBlock, def harness.CapabilityDeclaration) harness.CapabilityDeclaration {
	decl := def
	if len(block.HarnessSlots) > 0 {
		decl.HarnessSlots = block.HarnessSlots
	}
	if len(block.CapabilityKeys) > 0 {
		decl.CapabilityKeys = block.CapabilityKeys
	}
	return decl
}
