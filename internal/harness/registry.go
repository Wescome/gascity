package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Errors returned by the registry. These are the structured, fail-closed
// outcomes named in the IS (AC-REG3, AC-REG4, AC-REG5). They are distinct
// sentinels so callers dispatch with errors.Is and surface the correct
// 422-class step failure (AC-FC1).
var (
	// ErrNoProviderForRequirements is the AC-REG3 fail-closed selection error:
	// no registered provider's capability declaration satisfies the step's
	// runtime_requirements (and has capacity). There is no default and no
	// fallthrough.
	ErrNoProviderForRequirements = errors.New("no_provider_for_requirements")
	// ErrIncompleteHarnessTuple is the AC-REG4 registration error: a provider
	// whose harness_slots does not contain all eight slots E,T,C,S,L,V,G,P.
	ErrIncompleteHarnessTuple = errors.New("incomplete_harness_tuple")
	// ErrUnknownCapabilityKey is the AC-REG5 registration error: a capability
	// key not drawn from the canonical twelve-key set.
	ErrUnknownCapabilityKey = errors.New("unknown_capability_key")
	// ErrProviderAlreadyRegistered guards against duplicate provider ids.
	ErrProviderAlreadyRegistered = errors.New("provider_already_registered")
)

// HarnessSlots is the full Harness Tuple H = (E,T,C,S,L,V,G,P) (IS Definitions,
// AC-REG4). A provider that does not declare all eight slots is rejected at
// registration.
var HarnessSlots = []string{"E", "T", "C", "S", "L", "V", "G", "P"}

// CanonicalCapabilityKeys is the single normative source for runtime_requirements
// and provider capability keys (IS Definitions / Open Question 4). No other key
// is valid; every value in a CapabilityDeclaration and every entry in a step's
// runtime_requirements MUST be drawn from this set.
var CanonicalCapabilityKeys = []string{
	"ai_reasoning",
	"model_routing",
	"workspace_write_scope",
	"command_exec",
	"file_materialize",
	"workspace_init",
	"dependency_prep",
	"session_archive",
	"snapshot_restore",
	"contract_evaluation",
	"tool_capability_probe",
	"backup_restore",
}

// CapabilityDeclaration is the machine-readable manifest a provider registers
// (AC-REG1): which Harness Tuple slots it covers and which runtime_requirements
// capability keys it can satisfy. Both are config reads, not runtime probes
// (AC-REG4).
type CapabilityDeclaration struct {
	// HarnessSlots is the Harness Tuple coverage. Must contain all eight slots
	// for registration to succeed (AC-REG4).
	HarnessSlots []string
	// CapabilityKeys is the provider's declared capability key set, drawn only
	// from CanonicalCapabilityKeys (AC-REG5).
	CapabilityKeys []string
}

// registration pairs a HarnessProvider with its capability declaration.
type registration struct {
	provider HarnessProvider
	decl     CapabilityDeclaration
	capSet   map[string]struct{}
}

// Registry maps provider_id -> HarnessProvider + CapabilityDeclaration (AC-REG1).
// It is safe for concurrent use. The registry lives in city configuration
// (Open Question 3): the Go registry is populated from the [provider.*] blocks
// of city.toml, and no judgement enters Go.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registration
}

// NewRegistry returns an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]registration)}
}

// Register adds a provider under id with its capability declaration. It rejects
// providers whose HarnessSlots do not contain all eight slots (AC-REG4) and
// providers declaring a capability key outside the canonical set (AC-REG5).
func (r *Registry) Register(id string, provider HarnessProvider, decl CapabilityDeclaration) error {
	if id == "" {
		return fmt.Errorf("%w: empty provider id", ErrIncompleteHarnessTuple)
	}
	if provider == nil {
		return fmt.Errorf("register %q: provider is nil", id)
	}
	if err := assertCompleteTuple(decl.HarnessSlots); err != nil {
		return fmt.Errorf("register %q: %w", id, err)
	}
	if err := assertCanonicalKeys(decl.CapabilityKeys); err != nil {
		return fmt.Errorf("register %q: %w", id, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[id]; exists {
		return fmt.Errorf("%w: %q", ErrProviderAlreadyRegistered, id)
	}
	capSet := make(map[string]struct{}, len(decl.CapabilityKeys))
	for _, k := range decl.CapabilityKeys {
		capSet[k] = struct{}{}
	}
	r.entries[id] = registration{provider: provider, decl: decl, capSet: capSet}
	return nil
}

// Get returns the provider registered under id, if any.
func (r *Registry) Get(id string) (HarnessProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	if !ok {
		return nil, false
	}
	return entry.provider, true
}

// Select returns the single provider whose capability declaration is a superset
// of requirements, drawn from allowedProviders, with capacity > 0 (AC-REG2). It
// fails closed with ErrNoProviderForRequirements when no provider matches
// (AC-REG3); there is no default and no fallthrough.
//
// Selection is the normative algorithm (IS §Provider selection algorithm):
//  1. requirements is the step's runtime_requirements key set.
//  2. allowedProviders is the city config allow-list (preference order).
//  3. capacity is read via Status (AC-LC9).
//  4. filter to providers whose declared keys ⊇ requirements AND capacity > 0.
//  5. empty filtered set → ErrNoProviderForRequirements.
//  6. exactly one → select it.
//  7. more than one → first in allow-list order (deterministic for replay).
func (r *Registry) Select(ctx context.Context, requirements, allowedProviders []string) (HarnessProvider, string, error) {
	r.mu.RLock()
	// Snapshot matching entries while holding the read lock; call Status
	// outside the lock so a slow provider does not block registration.
	type candidate struct {
		id       string
		provider HarnessProvider
	}
	var candidates []candidate
	for _, id := range allowedProviders {
		entry, ok := r.entries[id]
		if !ok {
			continue // unregistered allow-list entries are ignored (AC-REG3)
		}
		if !supersetOf(entry.capSet, requirements) {
			continue
		}
		candidates = append(candidates, candidate{id: id, provider: entry.provider})
	}
	r.mu.RUnlock()

	for _, c := range candidates {
		status, err := c.provider.Status(ctx, StatusRequest{})
		if err != nil {
			// A provider whose status cannot be read is not eligible; fail
			// closed rather than guessing capacity.
			continue
		}
		if status.Capacity > 0 {
			return c.provider, c.id, nil
		}
	}

	return nil, "", fmt.Errorf("%w: requirements=%v allowed=%v", ErrNoProviderForRequirements, requirements, allowedProviders)
}

// supersetOf reports whether capSet contains every requirement.
func supersetOf(capSet map[string]struct{}, requirements []string) bool {
	for _, req := range requirements {
		if _, ok := capSet[req]; !ok {
			return false
		}
	}
	return true
}

// assertCompleteTuple enforces AC-REG4: the declared slots must contain all
// eight Harness Tuple slots.
func assertCompleteTuple(slots []string) error {
	present := make(map[string]struct{}, len(slots))
	for _, s := range slots {
		present[s] = struct{}{}
	}
	var missing []string
	for _, required := range HarnessSlots {
		if _, ok := present[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: missing slots %v", ErrIncompleteHarnessTuple, missing)
	}
	return nil
}

// assertCanonicalKeys enforces AC-REG5: every capability key must be drawn from
// the canonical twelve-key set.
func assertCanonicalKeys(keys []string) error {
	canonical := make(map[string]struct{}, len(CanonicalCapabilityKeys))
	for _, k := range CanonicalCapabilityKeys {
		canonical[k] = struct{}{}
	}
	for _, k := range keys {
		if _, ok := canonical[k]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownCapabilityKey, k)
		}
	}
	return nil
}
