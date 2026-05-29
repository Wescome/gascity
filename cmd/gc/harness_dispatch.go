package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/harness"
	"github.com/gastownhall/gascity/internal/harness/registrybuilder"
	"github.com/gastownhall/gascity/internal/molecule"
)

// harnessDispatchAttempted reports whether a bead carried harness
// runtime_requirements and was routed through harness dispatch. When true the
// caller MUST NOT fall through to the legacy dispatch path: a step with
// requirements either runs through a selected provider or fails closed
// (IS-GC-RUNTIME-PROVIDER-CONTRACT AC-FC1, AC-REG3).
type harnessDispatchOutcome struct {
	// Attempted is true when the bead declared runtime_requirements.
	Attempted bool
	// Selected is the provider id chosen by the registry, empty on failure.
	Selected string
	// Response is the provider execution response, nil on failure.
	Response *harness.ExecutionResponse
}

// harnessRegistryCache memoizes one harness.Registry per city-config provider
// signature so the control-dispatcher reuses a single registry across steps
// rather than rebuilding it on every dispatch (Step 5: build once at city
// start). The cache is keyed by a deterministic signature of the [provider.*]
// blocks so a config reload that changes provider blocks transparently rebuilds.
var harnessRegistryCache sync.Map // signature string -> *harness.Registry

// harnessRegistryForConfig returns the harness registry for the city config,
// building and caching it on first use. It returns (nil, nil) when the city
// declares no [provider.*] blocks: a city with no harness providers has no
// registry and any step that declares runtime_requirements fails closed.
func harnessRegistryForConfig(cfg *config.City) (*harness.Registry, error) {
	if cfg == nil {
		return nil, nil
	}
	blocks := cfg.HarnessProviderBlocks()
	if len(blocks) == 0 {
		return nil, nil
	}
	sig := harnessProviderSignature(blocks)
	if cached, ok := harnessRegistryCache.Load(sig); ok {
		return cached.(*harness.Registry), nil
	}
	registry, err := registrybuilder.Build(harnessProviderBlocks(blocks))
	if err != nil {
		return nil, fmt.Errorf("building harness registry: %w", err)
	}
	actual, _ := harnessRegistryCache.LoadOrStore(sig, registry)
	return actual.(*harness.Registry), nil
}

// harnessProviderBlocks maps the neutral config descriptors to the registry
// builder's block type. The builder package intentionally does not depend on
// internal/config (it keeps the harness kernel domain-neutral), so cmd/gc owns
// the mapping.
func harnessProviderBlocks(blocks []config.HarnessProviderBlock) []registrybuilder.ProviderBlock {
	out := make([]registrybuilder.ProviderBlock, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, registrybuilder.ProviderBlock{
			ID:             block.ID,
			URL:            block.URL,
			Token:          block.Token,
			HarnessSlots:   block.HarnessSlots,
			CapabilityKeys: block.CapabilityKeys,
		})
	}
	return out
}

// harnessProviderSignature builds a deterministic cache key from the provider
// blocks. Blocks arrive sorted by id (HarnessProviderBlocks), so the signature
// is stable for the same config and changes when any provider block changes.
func harnessProviderSignature(blocks []config.HarnessProviderBlock) string {
	var sb strings.Builder
	for _, block := range blocks {
		sb.WriteString(block.ID)
		sb.WriteByte('|')
		sb.WriteString(block.URL)
		sb.WriteByte('|')
		sb.WriteString(strings.Join(block.HarnessSlots, ","))
		sb.WriteByte('|')
		sb.WriteString(strings.Join(block.CapabilityKeys, ","))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// harnessAllowedProviders returns the city's provider allow-list in
// deterministic preference order (AC-REG2). The allow-list is the set of
// declared [provider.*] block ids, sorted, matching HarnessProviderBlocks so
// selection is reproducible for replay.
func harnessAllowedProviders(cfg *config.City) []string {
	blocks := cfg.HarnessProviderBlocks()
	allowed := make([]string, 0, len(blocks))
	for _, block := range blocks {
		allowed = append(allowed, block.ID)
	}
	sort.Strings(allowed)
	return allowed
}

// beadRuntimeRequirements reads the comma-separated harness runtime_requirements
// stamped onto a bead at instantiation (molecule.RuntimeRequirementsMetadataKey).
// It returns nil when the bead declares none, which means the step continues
// through the existing (non-harness) dispatch path.
func beadRuntimeRequirements(bead beads.Bead) []string {
	raw := strings.TrimSpace(bead.Metadata[molecule.RuntimeRequirementsMetadataKey])
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	reqs := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}
		reqs = append(reqs, key)
	}
	if len(reqs) == 0 {
		return nil
	}
	return reqs
}

// maybeDispatchHarness routes a bead with declared runtime_requirements through
// the harness provider registry. It is the additive harness dispatch path: a
// bead WITHOUT runtime_requirements returns Attempted=false and the caller
// proceeds through the existing dispatch path unchanged; a bead WITH
// runtime_requirements is dispatched here and the caller MUST NOT fall through.
//
// Fail-closed (AC-FC1, AC-REG3): when no provider's capability declaration is a
// superset of the requirements, the step is marked failed and the function
// returns an error wrapping harness.ErrNoProviderForRequirements. It never
// silently falls through to the legacy path.
func maybeDispatchHarness(ctx context.Context, store beads.Store, bead beads.Bead, cfg *config.City, cityPath string, stderr io.Writer) (harnessDispatchOutcome, error) {
	reqs := beadRuntimeRequirements(bead)
	if len(reqs) == 0 {
		return harnessDispatchOutcome{Attempted: false}, nil
	}

	registry, err := harnessRegistryForConfig(cfg)
	if err != nil {
		failHarnessStepClosed(store, bead.ID, "harness_registry_build_failed", err.Error(), stderr)
		return harnessDispatchOutcome{Attempted: true}, fmt.Errorf("harness dispatch bead=%s: %w", bead.ID, err)
	}
	if registry == nil {
		// A step declared requirements but the city declares no harness
		// providers. Fail closed rather than running it on an unknown runtime.
		failHarnessStepClosed(store, bead.ID, "no_harness_providers_configured", fmt.Sprintf("requirements=%v", reqs), stderr)
		return harnessDispatchOutcome{Attempted: true}, fmt.Errorf("harness dispatch bead=%s: %w: no [provider.*] blocks configured", bead.ID, harness.ErrNoProviderForRequirements)
	}

	allowed := harnessAllowedProviders(cfg)
	provider, providerID, selErr := registry.Select(ctx, reqs, allowed)
	if selErr != nil {
		failHarnessStepClosed(store, bead.ID, "no_provider_for_requirements", selErr.Error(), stderr)
		return harnessDispatchOutcome{Attempted: true}, fmt.Errorf("harness dispatch bead=%s: %w", bead.ID, selErr)
	}

	req := harnessExecutionRequestForBead(bead, cfg, cityPath, reqs)
	resp, execErr := provider.ExecuteStep(ctx, req)
	if execErr != nil {
		failHarnessStepClosed(store, bead.ID, "provider_execute_failed", execErr.Error(), stderr)
		return harnessDispatchOutcome{Attempted: true, Selected: providerID}, fmt.Errorf("harness dispatch bead=%s provider=%s: %w", bead.ID, providerID, execErr)
	}

	// Record the selected provider and status on the bead for replay-identity,
	// then feed the provider response into the fidelity validator. The validator
	// is the molecule verdict (release / revise / fail-closed); the provider
	// status is only an execution signal (AC-RS2).
	_, _ = fmt.Fprintf(stderr, "harness dispatch: bead=%s provider=%s status=%s\n", bead.ID, providerID, resp.Status)
	recordHarnessSelection(store, bead.ID, providerID, resp)

	if fidErr := runFidelityValidator(ctx, store, bead, cfg, cityPath, resp, stderr); fidErr != nil {
		return harnessDispatchOutcome{Attempted: true, Selected: providerID, Response: &resp}, fmt.Errorf("harness dispatch bead=%s provider=%s: %w", bead.ID, providerID, fidErr)
	}

	return harnessDispatchOutcome{Attempted: true, Selected: providerID, Response: &resp}, nil
}

// harnessExecutionRequestForBead populates the provider execution envelope from
// the bead's lineage metadata (AC-RQ1, AC-RQ2). Lineage pointers are carried
// opaquely; this function never interprets them as Factory categories.
func harnessExecutionRequestForBead(bead beads.Bead, cfg *config.City, cityPath string, reqs []string) harness.ExecutionRequest {
	cityID := loadedCityName(cfg, cityPath)
	declaredOutputs := harnessDeclaredOutputsForBead(bead)
	return harness.ExecutionRequest{
		CityID:          cityID,
		SessionID:       bead.Metadata["gc.session_id"],
		FormulaID:       bead.Metadata["gc.formula_id"],
		FormulaVersion:  bead.Metadata["gc.formula_version"],
		MoleculeID:      bead.Metadata["gc.root_bead_id"],
		BeadID:          bead.ID,
		StepName:        harnessStepName(bead),
		RoleName:        bead.Assignee,
		Purpose:         bead.Title,
		DeclaredOutputs: declaredOutputs,
		RuntimeConfig:   map[string]any{},
		Policy:          harnessPolicyForRequirements(reqs),
		ContextRefs: harness.ContextRefs{
			FnID: bead.Metadata["gc.fn_id"],
			IsID: bead.Metadata["gc.is_id"],
			EsID: bead.Metadata["gc.es_id"],
			EpID: bead.Metadata["gc.ep_id"],
		},
		VerifierContract: harnessVerifierContractForOutputs(declaredOutputs),
		IdempotencyKey:   harnessIdempotencyKey(bead),
	}
}

func harnessDeclaredOutputsForBead(bead beads.Bead) []string {
	if raw := strings.TrimSpace(bead.Metadata["gc.declared_outputs"]); raw != "" {
		if outputs := parseHarnessOutputList(raw); len(outputs) > 0 {
			return outputs
		}
	}
	const marker = "**Expected outputs:**"
	for _, line := range strings.Split(bead.Description, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, marker) {
			continue
		}
		if outputs := parseHarnessOutputList(strings.TrimSpace(strings.TrimPrefix(line, marker))); len(outputs) > 0 {
			return outputs
		}
	}
	return nil
}

func parseHarnessOutputList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var outputs []string
	if err := json.Unmarshal([]byte(raw), &outputs); err == nil {
		return compactHarnessOutputs(outputs)
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"'[]`)
	}
	return compactHarnessOutputs(parts)
}

func compactHarnessOutputs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func harnessVerifierContractForOutputs(outputs []string) *harness.VerifierContract {
	contract := &harness.VerifierContract{ExpectsInternalVerification: true}
	for _, output := range outputs {
		contract.DeclaredOutputMatch = append(contract.DeclaredOutputMatch, harness.OutputMatchRule{Artifact: output, Kind: "text"})
	}
	return contract
}

func harnessPolicyForRequirements(reqs []string) *harness.Policy {
	policy := &harness.Policy{FilesystemScope: []string{"/workspace"}}
	for _, req := range reqs {
		switch strings.TrimSpace(req) {
		case "command_exec":
			policy.ToolAllowlist = append(policy.ToolAllowlist, "command_exec")
		case "file_materialize", "workspace_write_scope":
			if len(policy.FilesystemScope) == 0 {
				policy.FilesystemScope = []string{"/workspace"}
			}
		}
	}
	return policy
}

func harnessStepName(bead beads.Bead) string {
	if ref := strings.TrimSpace(bead.Metadata["gc.step_ref"]); ref != "" {
		return ref
	}
	if ref := strings.TrimSpace(bead.Metadata["gc.step_id"]); ref != "" {
		return ref
	}
	return bead.Ref
}

func harnessIdempotencyKey(bead beads.Bead) string {
	if key := strings.TrimSpace(bead.Metadata["gc.idempotency_key"]); key != "" {
		return key
	}
	if key := strings.TrimSpace(bead.Metadata["idempotency_key"]); key != "" {
		return key
	}
	return bead.ID
}

// failHarnessStepClosed marks a bead failed-closed when harness dispatch cannot
// place the step on a provider (AC-FC1). It mirrors the quarantine failure
// stamping convention: status=closed, gc.outcome=fail, gc.failure_class=hard.
func failHarnessStepClosed(store beads.Store, beadID, failureReason, detail string, stderr io.Writer) {
	closed := "closed"
	metadata := map[string]string{
		"gc.outcome":                "fail",
		"gc.failure_class":          "hard",
		"gc.failure_reason":         failureReason,
		"gc.harness_dispatch_state": "fail_closed",
		"gc.harness_failure_detail": truncateHarnessDetail(detail),
		"gc.harness_failed_at":      time.Now().UTC().Format(time.RFC3339),
	}
	if err := store.Update(beadID, beads.UpdateOpts{
		Status:   &closed,
		Labels:   []string{"gc:harness-fail-closed"},
		Metadata: metadata,
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "harness dispatch: bead=%s fail-closed update error: %v\n", beadID, err)
	}
}

// recordHarnessSelection stamps the selected provider and verdict onto the bead
// for replay-identity. Best-effort: a metadata write failure is logged but does
// not undo a successful provider execution.
func recordHarnessSelection(store beads.Store, beadID, providerID string, resp harness.ExecutionResponse) {
	_ = store.Update(beadID, beads.UpdateOpts{
		Metadata: map[string]string{
			"gc.harness_provider_id":     providerID,
			"gc.harness_provider_status": string(resp.Status),
			"gc.harness_provider_id_at":  time.Now().UTC().Format(time.RFC3339),
		},
	})
}

const maxHarnessDetailMetadata = 512

func truncateHarnessDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if len(detail) <= maxHarnessDetailMetadata {
		return detail
	}
	return detail[:maxHarnessDetailMetadata]
}

// ensure errors stays referenced if future edits drop the only use.
var _ = errors.Is
