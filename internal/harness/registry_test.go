package harness

import (
	"context"
	"errors"
	"testing"
)

// stubProvider is a minimal HarnessProvider test double whose Status capacity
// is configurable so registry selection (AC-REG2) can be exercised.
type stubProvider struct {
	capacity int
}

func (s *stubProvider) CreateSession(context.Context, CreateSessionRequest) (CreateSessionResponse, error) {
	return CreateSessionResponse{Status: "ok"}, nil
}
func (s *stubProvider) PrepareWorkspace(context.Context, PrepareWorkspaceRequest) (PrepareWorkspaceResponse, error) {
	return PrepareWorkspaceResponse{Status: "ok"}, nil
}
func (s *stubProvider) ExecuteStep(context.Context, ExecutionRequest) (ExecutionResponse, error) {
	return ExecutionResponse{Status: StatusCompleted, PolicyEvents: []PolicyEvent{}}, nil
}
func (s *stubProvider) CollectArtifacts(context.Context, CollectArtifactsRequest) (CollectArtifactsResponse, error) {
	return CollectArtifactsResponse{}, nil
}
func (s *stubProvider) CollectLogs(context.Context, CollectLogsRequest) (CollectLogsResponse, error) {
	return CollectLogsResponse{}, nil
}
func (s *stubProvider) CollectPolicyEvents(context.Context, CollectPolicyEventsRequest) (CollectPolicyEventsResponse, error) {
	return CollectPolicyEventsResponse{PolicyEvents: []PolicyEvent{}}, nil
}
func (s *stubProvider) Snapshot(context.Context, SnapshotRequest) (SnapshotResponse, error) {
	return SnapshotResponse{Unsupported: true}, nil
}
func (s *stubProvider) Restore(context.Context, RestoreRequest) (RestoreResponse, error) {
	return RestoreResponse{Unsupported: true}, nil
}
func (s *stubProvider) Status(context.Context, StatusRequest) (StatusResponse, error) {
	return StatusResponse{Healthy: true, Ready: true, ProviderVersion: "test", Capacity: s.capacity}, nil
}
func (s *stubProvider) Restart(context.Context, RestartRequest) (RestartResponse, error) {
	return RestartResponse{Status: "ok"}, nil
}
func (s *stubProvider) Destroy(context.Context, DestroyRequest) (DestroyResponse, error) {
	return DestroyResponse{Status: "ok"}, nil
}

func allSlots() []string { return []string{"E", "T", "C", "S", "L", "V", "G", "P"} }

// AC-REG4: a provider whose harness_slots omit any of the eight slots is
// rejected at registration with ErrIncompleteHarnessTuple.
func TestRegister_RejectsIncompleteHarnessTuple(t *testing.T) {
	r := NewRegistry()
	err := r.Register("executor-only", &stubProvider{capacity: 1}, CapabilityDeclaration{
		HarnessSlots:   []string{"E"},
		CapabilityKeys: []string{"command_exec"},
	})
	if !errors.Is(err, ErrIncompleteHarnessTuple) {
		t.Fatalf("expected ErrIncompleteHarnessTuple, got %v", err)
	}
}

// AC-REG4: a tuple-complete provider registers successfully.
func TestRegister_AcceptsCompleteTuple(t *testing.T) {
	r := NewRegistry()
	err := r.Register("pi-rpc", &stubProvider{capacity: 1}, CapabilityDeclaration{
		HarnessSlots:   allSlots(),
		CapabilityKeys: []string{"ai_reasoning", "model_routing"},
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// AC-REG5: capability keys must come from the canonical twelve-key set.
func TestRegister_RejectsUnknownCapabilityKey(t *testing.T) {
	r := NewRegistry()
	err := r.Register("bad", &stubProvider{capacity: 1}, CapabilityDeclaration{
		HarnessSlots:   allSlots(),
		CapabilityKeys: []string{"deploy_to_prod"},
	})
	if !errors.Is(err, ErrUnknownCapabilityKey) {
		t.Fatalf("expected ErrUnknownCapabilityKey, got %v", err)
	}
}

// AC-REG2/AC-REG3: selection returns the single provider whose declaration is a
// superset of the requirements.
func TestSelect_MatchesSuperset(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "pi-rpc", &stubProvider{capacity: 2}, []string{"ai_reasoning", "model_routing", "workspace_write_scope"})

	p, id, err := r.Select(context.Background(), []string{"ai_reasoning"}, []string{"pi-rpc"})
	if err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	if id != "pi-rpc" || p == nil {
		t.Fatalf("expected pi-rpc, got id=%q provider=%v", id, p)
	}
}

// AC-REG3 / AC-FC1: fail closed when no provider satisfies the requirements.
func TestSelect_FailsClosedOnNoMatch(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "cloudflare-sandbox", &stubProvider{capacity: 1}, []string{"command_exec", "file_materialize"})

	_, _, err := r.Select(context.Background(), []string{"ai_reasoning"}, []string{"cloudflare-sandbox"})
	if !errors.Is(err, ErrNoProviderForRequirements) {
		t.Fatalf("expected ErrNoProviderForRequirements, got %v", err)
	}
}

// AC-REG6: a step requiring ai_reasoning MUST NOT select cloudflare-sandbox.
func TestSelect_DoesNotSelectSandboxForAIReasoning(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "pi-rpc", &stubProvider{capacity: 1}, []string{"ai_reasoning", "model_routing"})
	mustRegister(t, r, "cloudflare-sandbox", &stubProvider{capacity: 1}, []string{"command_exec", "file_materialize", "workspace_init", "dependency_prep", "backup_restore"})

	_, id, err := r.Select(context.Background(), []string{"ai_reasoning"}, []string{"cloudflare-sandbox", "pi-rpc"})
	if err != nil {
		t.Fatalf("expected a match, got %v", err)
	}
	if id != "pi-rpc" {
		t.Fatalf("ai_reasoning must route to pi-rpc, got %q", id)
	}
}

// AC-REG6: a setup/teardown-only step MAY select cloudflare-sandbox.
func TestSelect_SetupOnlyMaySelectSandbox(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "cloudflare-sandbox", &stubProvider{capacity: 1}, []string{"command_exec", "file_materialize", "workspace_init", "dependency_prep"})

	_, id, err := r.Select(context.Background(), []string{"command_exec", "workspace_init"}, []string{"cloudflare-sandbox"})
	if err != nil {
		t.Fatalf("expected a match, got %v", err)
	}
	if id != "cloudflare-sandbox" {
		t.Fatalf("expected cloudflare-sandbox, got %q", id)
	}
}

// AC-REG2: a provider with zero capacity is filtered out of selection.
func TestSelect_FiltersZeroCapacity(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "pi-rpc", &stubProvider{capacity: 0}, []string{"ai_reasoning"})

	_, _, err := r.Select(context.Background(), []string{"ai_reasoning"}, []string{"pi-rpc"})
	if !errors.Is(err, ErrNoProviderForRequirements) {
		t.Fatalf("expected fail-closed on zero capacity, got %v", err)
	}
}

// AC-REG2: with multiple matches, selection is deterministic by the allow-list
// preference order.
func TestSelect_DeterministicByPreferenceOrder(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "pi-rpc", &stubProvider{capacity: 1}, []string{"command_exec"})
	mustRegister(t, r, "cloudflare-sandbox", &stubProvider{capacity: 1}, []string{"command_exec"})

	_, id, err := r.Select(context.Background(), []string{"command_exec"}, []string{"cloudflare-sandbox", "pi-rpc"})
	if err != nil {
		t.Fatalf("expected a match, got %v", err)
	}
	if id != "cloudflare-sandbox" {
		t.Fatalf("expected first-listed cloudflare-sandbox, got %q", id)
	}
}

// AC-REG3: an allow-list naming an unregistered provider does not crash and
// fails closed.
func TestSelect_IgnoresUnregisteredAllowListEntries(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "pi-rpc", &stubProvider{capacity: 1}, []string{"ai_reasoning"})

	_, _, err := r.Select(context.Background(), []string{"ai_reasoning"}, []string{"ghost-provider"})
	if !errors.Is(err, ErrNoProviderForRequirements) {
		t.Fatalf("expected fail-closed, got %v", err)
	}
}

func mustRegister(t *testing.T, r *Registry, id string, p HarnessProvider, caps []string) {
	t.Helper()
	if err := r.Register(id, p, CapabilityDeclaration{HarnessSlots: allSlots(), CapabilityKeys: caps}); err != nil {
		t.Fatalf("register %q: %v", id, err)
	}
}
