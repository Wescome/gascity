package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/harness"
	"github.com/gastownhall/gascity/internal/molecule"
)

// fakeHarnessProvider is a stub HarnessProvider used to exercise the
// control-dispatcher harness path without a live runtime. It records the
// ExecuteStep request and reports a fixed capacity.
type fakeHarnessProvider struct {
	capacity     int
	executeCalls []harness.ExecutionRequest
}

func (f *fakeHarnessProvider) CreateSession(context.Context, harness.CreateSessionRequest) (harness.CreateSessionResponse, error) {
	return harness.CreateSessionResponse{Status: "ok"}, nil
}
func (f *fakeHarnessProvider) PrepareWorkspace(context.Context, harness.PrepareWorkspaceRequest) (harness.PrepareWorkspaceResponse, error) {
	return harness.PrepareWorkspaceResponse{Status: "ok"}, nil
}
func (f *fakeHarnessProvider) ExecuteStep(_ context.Context, req harness.ExecutionRequest) (harness.ExecutionResponse, error) {
	f.executeCalls = append(f.executeCalls, req)
	return harness.ExecutionResponse{Status: harness.StatusCompleted}, nil
}
func (f *fakeHarnessProvider) CollectArtifacts(context.Context, harness.CollectArtifactsRequest) (harness.CollectArtifactsResponse, error) {
	return harness.CollectArtifactsResponse{}, nil
}
func (f *fakeHarnessProvider) CollectLogs(context.Context, harness.CollectLogsRequest) (harness.CollectLogsResponse, error) {
	return harness.CollectLogsResponse{}, nil
}
func (f *fakeHarnessProvider) CollectPolicyEvents(context.Context, harness.CollectPolicyEventsRequest) (harness.CollectPolicyEventsResponse, error) {
	return harness.CollectPolicyEventsResponse{PolicyEvents: []harness.PolicyEvent{}}, nil
}
func (f *fakeHarnessProvider) Snapshot(context.Context, harness.SnapshotRequest) (harness.SnapshotResponse, error) {
	return harness.SnapshotResponse{Unsupported: true}, nil
}
func (f *fakeHarnessProvider) Restore(context.Context, harness.RestoreRequest) (harness.RestoreResponse, error) {
	return harness.RestoreResponse{Unsupported: true}, nil
}
func (f *fakeHarnessProvider) Status(context.Context, harness.StatusRequest) (harness.StatusResponse, error) {
	return harness.StatusResponse{Healthy: true, Ready: true, Capacity: f.capacity}, nil
}
func (f *fakeHarnessProvider) Restart(context.Context, harness.RestartRequest) (harness.RestartResponse, error) {
	return harness.RestartResponse{Status: "ok"}, nil
}
func (f *fakeHarnessProvider) Destroy(context.Context, harness.DestroyRequest) (harness.DestroyResponse, error) {
	return harness.DestroyResponse{Status: "ok"}, nil
}

// seedHarnessRegistry registers a fake provider with the given capability keys
// and pre-seeds the dispatch cache so harnessRegistryForConfig returns it for
// the given config. It returns the config and a cleanup that clears the cache
// entry so tests do not leak across runs.
func seedHarnessRegistry(t *testing.T, providerID string, capabilityKeys []string, capacity int) (*config.City, *fakeHarnessProvider, func()) {
	t.Helper()
	provider := &fakeHarnessProvider{capacity: capacity}
	registry := harness.NewRegistry()
	if err := registry.Register(providerID, provider, harness.CapabilityDeclaration{
		HarnessSlots:   harness.HarnessSlots,
		CapabilityKeys: capabilityKeys,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := &config.City{
		Provider: map[string]config.ProviderSpec{
			providerID: {
				URL:            "https://example.invalid",
				HarnessSlots:   harness.HarnessSlots,
				CapabilityKeys: capabilityKeys,
			},
		},
	}
	sig := harnessProviderSignature(cfg.HarnessProviderBlocks())
	harnessRegistryCache.Store(sig, registry)
	cleanup := func() { harnessRegistryCache.Delete(sig) }
	return cfg, provider, cleanup
}

// A step with no runtime_requirements is not attempted: the caller proceeds
// through the existing dispatch path unchanged (additive contract).
func TestMaybeDispatchHarnessSkipsBeadWithoutRequirements(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "Plain step", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cfg, _, cleanup := seedHarnessRegistry(t, "pi-rpc", []string{"ai_reasoning"}, 1)
	defer cleanup()

	outcome, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maybeDispatchHarness: %v", err)
	}
	if outcome.Attempted {
		t.Fatalf("expected Attempted=false for a bead without runtime_requirements")
	}
}

// A step whose requirements are satisfied selects the provider and runs
// ExecuteStep with the populated request envelope.
func TestMaybeDispatchHarnessSelectsProviderAndExecutes(t *testing.T) {
	// A successful provider execution now flows into the fidelity validator;
	// point it at a fake release driver that POSTs RELEASE (exit 0) so this test
	// exercises only the selection + execution path.
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	cfg, provider, cleanup := seedHarnessRegistry(t, "pi-rpc", []string{"ai_reasoning", "model_routing", "command_exec"}, 1)
	defer cleanup()

	bead, err := store.Create(beads.Bead{
		Title: "Implement change",
		Type:  "task",
		Metadata: map[string]string{
			molecule.RuntimeRequirementsMetadataKey: "ai_reasoning,model_routing",
			"gc.step_ref":                           "implement",
			"gc.root_bead_id":                       "mol-1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("maybeDispatchHarness: %v", err)
	}
	if !outcome.Attempted {
		t.Fatalf("expected Attempted=true for a bead with runtime_requirements")
	}
	if outcome.Selected != "pi-rpc" {
		t.Fatalf("Selected = %q, want pi-rpc", outcome.Selected)
	}
	if len(provider.executeCalls) != 1 {
		t.Fatalf("ExecuteStep called %d times, want 1", len(provider.executeCalls))
	}
	got := provider.executeCalls[0]
	if got.BeadID != bead.ID {
		t.Errorf("ExecuteStep BeadID = %q, want %q", got.BeadID, bead.ID)
	}
	if got.StepName != "implement" {
		t.Errorf("ExecuteStep StepName = %q, want implement", got.StepName)
	}
	if got.MoleculeID != "mol-1" {
		t.Errorf("ExecuteStep MoleculeID = %q, want mol-1", got.MoleculeID)
	}
	if got.SessionID != "mol-1" {
		t.Errorf("ExecuteStep SessionID = %q, want root fallback mol-1", got.SessionID)
	}
	if got.VerifierContract == nil || got.VerifierContract.IsEmpty() {
		t.Fatalf("ExecuteStep VerifierContract is empty")
	}
	// The selected provider is recorded on the bead for replay-identity.
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Metadata["gc.harness_provider_id"] != "pi-rpc" {
		t.Errorf("gc.harness_provider_id = %q, want pi-rpc", updated.Metadata["gc.harness_provider_id"])
	}
}

func TestHarnessExecutionRequestExtractsDeclaredOutputs(t *testing.T) {
	bead := beads.Bead{
		ID:          "gc-1",
		Title:       "Plan",
		Description: "**Expected outputs:** [\"PLAN.md\"]\n",
		Metadata: map[string]string{
			"gc.root_bead_id": "gc-root",
			"gc.step_ref":     "factory.plan",
		},
	}
	req := harnessExecutionRequestForBead(bead, &config.City{}, t.TempDir(), []string{"workspace_write_scope"})
	if len(req.DeclaredOutputs) != 1 || req.DeclaredOutputs[0] != "PLAN.md" {
		t.Fatalf("DeclaredOutputs = %#v, want PLAN.md", req.DeclaredOutputs)
	}
	if req.VerifierContract == nil || req.VerifierContract.IsEmpty() {
		t.Fatalf("VerifierContract is empty")
	}
	if req.Policy == nil || req.Policy.IsEmpty() {
		t.Fatalf("Policy is empty")
	}
}

// A step whose requirements match no provider fails closed: the bead is marked
// failed and the error wraps ErrNoProviderForRequirements. It never silently
// falls through to the legacy path.
func TestMaybeDispatchHarnessFailsClosedWhenNoProviderMatches(t *testing.T) {
	store := beads.NewMemStore()
	// Provider declares only ai_reasoning; the step needs a key it cannot satisfy.
	cfg, provider, cleanup := seedHarnessRegistry(t, "pi-rpc", []string{"ai_reasoning"}, 1)
	defer cleanup()

	bead, err := store.Create(beads.Bead{
		Title: "Needs unsatisfiable capability",
		Type:  "task",
		Metadata: map[string]string{
			molecule.RuntimeRequirementsMetadataKey: "ai_reasoning,snapshot_restore",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard)
	if !outcome.Attempted {
		t.Fatalf("expected Attempted=true for a bead with runtime_requirements")
	}
	if !errors.Is(err, harness.ErrNoProviderForRequirements) {
		t.Fatalf("expected ErrNoProviderForRequirements, got %v", err)
	}
	if len(provider.executeCalls) != 0 {
		t.Fatalf("ExecuteStep must not run when selection fails closed, got %d calls", len(provider.executeCalls))
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status != "closed" {
		t.Errorf("status = %q, want closed (fail-closed)", updated.Status)
	}
	if updated.Metadata["gc.outcome"] != "fail" {
		t.Errorf("gc.outcome = %q, want fail", updated.Metadata["gc.outcome"])
	}
	if updated.Metadata["gc.harness_dispatch_state"] != "fail_closed" {
		t.Errorf("gc.harness_dispatch_state = %q, want fail_closed", updated.Metadata["gc.harness_dispatch_state"])
	}
}

// A step that declares requirements in a city with no [provider.*] blocks fails
// closed rather than running on an unknown runtime.
func TestMaybeDispatchHarnessFailsClosedWhenNoProvidersConfigured(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{}

	bead, err := store.Create(beads.Bead{
		Title: "Needs harness but city has none",
		Type:  "task",
		Metadata: map[string]string{
			molecule.RuntimeRequirementsMetadataKey: "ai_reasoning",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard)
	if !outcome.Attempted {
		t.Fatalf("expected Attempted=true")
	}
	if !errors.Is(err, harness.ErrNoProviderForRequirements) {
		t.Fatalf("expected ErrNoProviderForRequirements, got %v", err)
	}
	updated, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status != "closed" {
		t.Errorf("status = %q, want closed", updated.Status)
	}
}
