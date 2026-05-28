package harness

import "context"

// HarnessProvider is the Gas City provider-contract façade (AC-LC1..11). It
// sits ABOVE the low-level runtime.Provider (PTY/session) interface. Each method
// is one of the eleven lifecycle obligations from the architecture reference
// §Provider Contract. Implementations bind these obligations to one or more real
// runtime.Provider methods (cloudflare-sandbox) or to an RPC turn (pi-rpc); the
// façade is the stable contract, the substrate beneath it is an implementation
// detail (IS §Interface binding).
type HarnessProvider interface {
	// CreateSession allocates a session before any step executes (AC-LC1).
	// Fail-closed: a non-OK status means Gas City does NOT proceed to
	// PrepareWorkspace.
	CreateSession(ctx context.Context, req CreateSessionRequest) (CreateSessionResponse, error)

	// PrepareWorkspace materializes inputs, dependency state, and applies the
	// policy filesystem scope (AC-LC2). Fail-closed: a missing input, an
	// unavailable required credential, or an unenforceable scope returns non-OK
	// and Gas City does NOT call ExecuteStep.
	PrepareWorkspace(ctx context.Context, req PrepareWorkspaceRequest) (PrepareWorkspaceResponse, error)

	// ExecuteStep runs the agent/job loop for one Formula step (AC-LC3). It is
	// the only method that runs E. Returns the full response envelope (AC-RS1).
	ExecuteStep(ctx context.Context, req ExecutionRequest) (ExecutionResponse, error)

	// CollectArtifacts returns produced files and the declared-vs-produced
	// manifest (AC-LC4). A checksum that cannot be computed marks the artifact
	// missing rather than reporting a false produced.
	CollectArtifacts(ctx context.Context, req CollectArtifactsRequest) (CollectArtifactsResponse, error)

	// CollectLogs returns stdout/stderr/trace spans (AC-LC5).
	CollectLogs(ctx context.Context, req CollectLogsRequest) (CollectLogsResponse, error)

	// CollectPolicyEvents returns allow/deny/escalation/violation events
	// (AC-LC6). A provider with no policy surface returns an empty slice — never
	// nil.
	CollectPolicyEvents(ctx context.Context, req CollectPolicyEventsRequest) (CollectPolicyEventsResponse, error)

	// Snapshot captures runtime state for replay/continuation (AC-LC7). A
	// provider that does not support snapshots sets Unsupported=true; it MUST NOT
	// return an error for the unsupported case.
	Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotResponse, error)

	// Restore restores runtime state from a snapshot (AC-LC8). A provider that
	// does not support restore sets Unsupported=true.
	Restore(ctx context.Context, req RestoreRequest) (RestoreResponse, error)

	// Status reports health, readiness, version, image digest, and capacity
	// (AC-LC9). The registry reads Capacity during selection (AC-REG2).
	Status(ctx context.Context, req StatusRequest) (StatusResponse, error)

	// Restart restarts a failed or stale runtime (AC-LC10). Fail-closed: a
	// restart that cannot bring the runtime healthy returns non-OK; Gas City
	// treats the step as timeout/failed and the bead stays open (AC-FC6).
	Restart(ctx context.Context, req RestartRequest) (RestartResponse, error)

	// Destroy releases runtime resources and revokes scoped credentials (AC-LC11).
	// Fail-closed for credentials: it MUST revoke any minted scoped credential
	// even if the runtime is already gone.
	Destroy(ctx context.Context, req DestroyRequest) (DestroyResponse, error)
}

// CreateSessionRequest is the AC-LC1 input.
type CreateSessionRequest struct {
	CityID     string `json:"city_id"`
	SessionID  string `json:"session_id"`
	FormulaID  string `json:"formula_id"`
	MoleculeID string `json:"molecule_id,omitempty"`
	BeadID     string `json:"bead_id,omitempty"`
}

// CreateSessionResponse is the AC-LC1 output.
type CreateSessionResponse struct {
	SessionHandle string `json:"session_handle"`
	Status        string `json:"status"`
}

// PrepareWorkspaceRequest is the AC-LC2 input.
type PrepareWorkspaceRequest struct {
	SessionHandle string            `json:"session_handle"`
	Inputs        map[string]string `json:"inputs"`
	Policy        *Policy           `json:"policy"`
	ContextRefs   ContextRefs       `json:"context_refs"`
}

// PrepareWorkspaceResponse is the AC-LC2 output.
type PrepareWorkspaceResponse struct {
	Status       string   `json:"status"`
	PreparedRefs []string `json:"prepared_refs,omitempty"`
}

// CollectArtifactsRequest is the AC-LC4 input.
type CollectArtifactsRequest struct {
	SessionHandle   string   `json:"session_handle"`
	DeclaredOutputs []string `json:"declared_outputs"`
}

// CollectArtifactsResponse is the AC-LC4 output.
type CollectArtifactsResponse struct {
	Artifacts        []Artifact      `json:"artifacts"`
	ArtifactManifest []ManifestEntry `json:"artifact_manifest"`
}

// CollectLogsRequest is the AC-LC5 input.
type CollectLogsRequest struct {
	SessionHandle string `json:"session_handle"`
}

// CollectLogsResponse is the AC-LC5 output.
type CollectLogsResponse struct {
	StdoutRef  string      `json:"stdout_ref,omitempty"`
	StderrRef  string      `json:"stderr_ref,omitempty"`
	TraceSpans []TraceSpan `json:"trace_spans,omitempty"`
}

// CollectPolicyEventsRequest is the AC-LC6 input.
type CollectPolicyEventsRequest struct {
	SessionHandle string `json:"session_handle"`
}

// CollectPolicyEventsResponse is the AC-LC6 output. PolicyEvents is never nil.
type CollectPolicyEventsResponse struct {
	PolicyEvents []PolicyEvent `json:"policy_events"`
}

// SnapshotRequest is the AC-LC7 input.
type SnapshotRequest struct {
	SessionHandle string `json:"session_handle"`
}

// SnapshotResponse is the AC-LC7 output. Unsupported is true when the provider
// does not support snapshots.
type SnapshotResponse struct {
	SnapshotRef string `json:"snapshot_ref,omitempty"`
	Unsupported bool   `json:"unsupported,omitempty"`
}

// RestoreRequest is the AC-LC8 input.
type RestoreRequest struct {
	SessionHandle string `json:"session_handle"`
	SnapshotRef   string `json:"snapshot_ref"`
}

// RestoreResponse is the AC-LC8 output. Unsupported is true when the provider
// does not support restore.
type RestoreResponse struct {
	Status      string `json:"status,omitempty"`
	Unsupported bool   `json:"unsupported,omitempty"`
}

// StatusRequest is the AC-LC9 input. SessionHandle is optional.
type StatusRequest struct {
	SessionHandle string `json:"session_handle,omitempty"`
}

// StatusResponse is the AC-LC9 output. Capacity is read by the registry during
// selection (AC-REG2): a provider with Capacity <= 0 is filtered out.
type StatusResponse struct {
	Healthy         bool   `json:"healthy"`
	Ready           bool   `json:"ready"`
	ProviderVersion string `json:"provider_version"`
	ImageDigest     string `json:"image_digest,omitempty"`
	Capacity        int    `json:"capacity"`
}

// RestartRequest is the AC-LC10 input.
type RestartRequest struct {
	SessionHandle string `json:"session_handle"`
	Reason        string `json:"reason"`
}

// RestartResponse is the AC-LC10 output.
type RestartResponse struct {
	Status string `json:"status"`
}

// DestroyRequest is the AC-LC11 input.
type DestroyRequest struct {
	SessionHandle string `json:"session_handle"`
}

// DestroyResponse is the AC-LC11 output.
type DestroyResponse struct {
	Status string `json:"status"`
}
