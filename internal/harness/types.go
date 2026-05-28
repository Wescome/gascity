// Package harness defines the Gas City harness runtime provider contract:
// the provider registry, the execution request/response envelopes, and the
// eleven lifecycle methods that sit ABOVE the low-level runtime.Provider
// (PTY/session) interface.
//
// Implements IS-GC-RUNTIME-PROVIDER-CONTRACT. The contract is Gas City domain
// language (provider, capability, purpose, policy, verifier contract); it does
// not name LLM-provider-specific concepts and never interprets context_refs as
// Factory categories (AC-RQ2, AC-RQ3).
package harness

// ProviderStatus is the execution-outcome enum carried on the response
// envelope (AC-RS1). It is the provider verdict — an execution-status signal —
// and is explicitly NOT the molecule verdict (AC-RS2). A provider MUST NOT emit
// "approved"/"revise" or any Factory coherence/fidelity verdict.
type ProviderStatus string

const (
	// StatusCompleted reports a step ran to a manifest-backed produced set.
	// A provider MUST NOT set this unless artifact_manifest shows every
	// declared_output as produced (AC-FC5).
	StatusCompleted ProviderStatus = "completed"
	// StatusFailed reports a step did not complete successfully.
	StatusFailed ProviderStatus = "failed"
	// StatusPolicyViolation reports the provider stopped execution because a
	// policy rule was violated mid-run (AC-FC4).
	StatusPolicyViolation ProviderStatus = "policy_violation"
	// StatusTimeout reports the provider exceeded its execution deadline
	// (AC-FC6). Gas City stores the session archive and the bead stays open.
	StatusTimeout ProviderStatus = "timeout"
	// StatusCancelled reports the step was cancelled before completion.
	StatusCancelled ProviderStatus = "cancelled"
)

// ManifestStatus classifies a declared output against the produced set
// (AC-RS3).
type ManifestStatus string

const (
	// ManifestProduced marks a declared output with a matching produced artifact.
	ManifestProduced ManifestStatus = "produced"
	// ManifestMissing marks a declared output with no matching produced
	// artifact, or an artifact whose checksum could not be computed (AC-LC4).
	ManifestMissing ManifestStatus = "missing"
	// ManifestExtra marks a produced artifact not present in declared_outputs.
	// It does not fail the provider (AC-RS3); the forensic write scope handles
	// unauthorized paths (AC-PI3).
	ManifestExtra ManifestStatus = "extra"
)

// PolicyEventKind classifies a governance event (AC-LC6, AC-FC4).
type PolicyEventKind string

const (
	// PolicyAllow records an allowed governed operation.
	PolicyAllow PolicyEventKind = "allow"
	// PolicyDeny records a denied governed operation.
	PolicyDeny PolicyEventKind = "deny"
	// PolicyEscalation records an operation that required escalation.
	PolicyEscalation PolicyEventKind = "escalation"
	// PolicyViolation records a policy violation that stops execution
	// (e.g. a filesystem_scope breach, AC-PI3).
	PolicyViolation PolicyEventKind = "violation"
)

// ExecutionRequest is the Gas City -> provider execution request envelope
// (AC-RQ1). Gas City sends exactly one per Formula step. runtime_config is the
// only provider-specific block; the rest of the envelope is stable across
// providers (AC-RQ4).
type ExecutionRequest struct {
	CityID         string `json:"city_id"`
	SessionID      string `json:"session_id"`
	FormulaID      string `json:"formula_id"`
	FormulaVersion string `json:"formula_version"`
	MoleculeID     string `json:"molecule_id"`
	BeadID         string `json:"bead_id"`
	StepName       string `json:"step_name"`
	RoleName       string `json:"role_name"`
	// Purpose is a narrowed natural-language statement of what THIS step must
	// accomplish (AC-RQ3). A provider MUST NOT broaden its tool/network/fs scope
	// beyond what policy grants for this purpose.
	Purpose string `json:"purpose"`
	// Inputs are the input artifacts / refs (AC-RQ1).
	Inputs map[string]string `json:"inputs"`
	// DeclaredOutputs are the declared output names matched in artifact_manifest.
	DeclaredOutputs []string `json:"declared_outputs"`
	// RuntimeConfig is the provider-specific config block (AC-RQ4). Opaque to
	// the envelope kernel; its shape is defined per provider.
	RuntimeConfig map[string]any `json:"runtime_config"`
	// Policy carries tool allowlist, network policy, and filesystem scope.
	// Absence fails closed (AC-FC3). This IS specifies the field shape; the
	// compiler that produces it is IS-GC-PROVIDER-POLICY-COMPILER.
	Policy *Policy `json:"policy"`
	// ContextRefs carries opaque lineage pointers (AC-RQ2). The provider carries
	// them through to evidence and MUST NOT interpret them as Factory categories.
	ContextRefs ContextRefs `json:"context_refs"`
	// VerifierContract declares what constitutes a valid output for the step.
	// Mandatory; absence fails closed (AC-FC2).
	VerifierContract *VerifierContract `json:"verifier_contract"`
	// IdempotencyKey is stable for a given (molecule, step, attempt) (AC-RQ5).
	IdempotencyKey string `json:"idempotency_key"`
}

// ContextRefs carries the lineage pointers sourced from the FORM-* vars at
// dispatch (AC-RQ2). Opaque purpose-binding (P slot); never interpreted.
type ContextRefs struct {
	FnID string `json:"fn_id"`
	IsID string `json:"is_id"`
	EsID string `json:"es_id"`
	EpID string `json:"ep_id"`
}

// Policy is the tool/network/filesystem scope envelope field (AC-RQ1, AC-FC3).
// The full compiler that produces it is out of scope (IS-GC-PROVIDER-POLICY-COMPILER);
// this is the field shape the provider reads and enforces.
type Policy struct {
	// ToolAllowlist names the tools the provider may use for this purpose.
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`
	// NetworkPolicy declares allowed network egress (deny-by-default).
	NetworkPolicy []string `json:"network_policy,omitempty"`
	// FilesystemScope declares the write-scope boundary (workspace_write_scope).
	FilesystemScope []string `json:"filesystem_scope,omitempty"`
}

// IsEmpty reports whether the policy carries no enforceable scope. An empty
// policy fails closed (AC-FC3): a provider MUST reject a request whose policy
// is nil or empty rather than execute.
func (p *Policy) IsEmpty() bool {
	if p == nil {
		return true
	}
	return len(p.ToolAllowlist) == 0 && len(p.NetworkPolicy) == 0 && len(p.FilesystemScope) == 0
}

// VerifierContract declares valid-output rules for a step (AC-RQ1). Mandatory;
// an empty contract fails closed (AC-FC2).
type VerifierContract struct {
	// DeclaredOutputMatch lists the declared-output match rules.
	DeclaredOutputMatch []OutputMatchRule `json:"declared_output_match,omitempty"`
	// ExpectsInternalVerification, when true, declares the provider MAY run
	// internal verification and report verifier_report_ref. The report is
	// evidence for Gas City fidelity validation, not the molecule verdict (AC-FC7).
	ExpectsInternalVerification bool `json:"expects_internal_verification,omitempty"`
}

// IsEmpty reports whether the verifier contract carries no rules. An empty
// contract fails closed (AC-FC2).
func (v *VerifierContract) IsEmpty() bool {
	if v == nil {
		return true
	}
	return len(v.DeclaredOutputMatch) == 0 && !v.ExpectsInternalVerification
}

// OutputMatchRule is one declared-output validity rule.
type OutputMatchRule struct {
	// Artifact is the declared output name this rule governs.
	Artifact string `json:"artifact"`
	// Kind is the contract kind (e.g. "exact_line", "json", "text", "markdown").
	Kind string `json:"kind"`
}

// ExecutionResponse is the provider -> Gas City execution response envelope
// (AC-RS1). A provider returns exactly one per ExecuteStep. Provider evidence
// flows from this envelope into Gas City fidelity validation (AC-EV1); the
// Factory sees only Gas City events, never these fields (AC-EV2).
type ExecutionResponse struct {
	// Status is the provider verdict — an execution-status signal (AC-RS2).
	Status ProviderStatus `json:"status"`
	// ProviderVerdict is the execution outcome. NOT the molecule verdict (AC-RS2).
	ProviderVerdict ProviderVerdict `json:"provider_verdict"`
	// Artifacts are the produced outputs with { path, size, checksum }.
	Artifacts []Artifact `json:"artifacts"`
	// ArtifactManifest maps each declared output to produced|missing|extra.
	ArtifactManifest []ManifestEntry `json:"artifact_manifest"`
	// Logs references stdout/stderr/trace spans.
	Logs Logs `json:"logs"`
	// PolicyEvents are allow|deny|escalation|violation events. Never nil — an
	// empty slice is contract-valid (AC-LC6).
	PolicyEvents []PolicyEvent `json:"policy_events"`
	// ModelUsage is populated for providers that perform inference (AC-RS6).
	// Nil for providers that do no inference (cloudflare-sandbox).
	ModelUsage *ModelUsage `json:"model_usage"`
	// RuntimeIdentity is the replay-identity anchor. Always present (AC-RS5).
	RuntimeIdentity RuntimeIdentity `json:"runtime_identity"`
	// SessionArchiveRef is a Gas City-owned reference to captured session state,
	// not the raw bytes (AC-RS7). Empty when no archive was captured.
	SessionArchiveRef string `json:"session_archive_ref,omitempty"`
	// VerifierReportRef references an internal verification report, if the
	// provider ran one. Evidence only, not the molecule verdict (AC-FC7).
	VerifierReportRef string `json:"verifier_report_ref,omitempty"`
	// Error is a structured error on non-completed status (AC-RS4). Nil when
	// status == completed.
	Error *ProviderError `json:"error"`
	// CompletionClaimedWithoutManifest is true iff completion was claimed by a
	// provider end-of-turn self-report rather than by a manifest-backed produced
	// set (AC-RS1, AC-FC5). The externally-verifiable stop-condition flag read by
	// Gas City fidelity validation (FV-09 check 4).
	CompletionClaimedWithoutManifest bool `json:"completion_claimed_without_manifest"`
	// StepOutputs is the domain-adapter-normalized per-step evidence map (AC-RS1).
	// Opaque to the envelope kernel; the sole channel for step-type-specific
	// evidence, keeping the contract kernel domain-neutral.
	StepOutputs map[string]any `json:"step_outputs,omitempty"`
}

// ProviderVerdict is the execution-outcome object (AC-RS1). It carries the
// status reason and is never the molecule verdict (AC-RS2).
type ProviderVerdict struct {
	// Status mirrors the envelope status enum.
	Status ProviderStatus `json:"status"`
	// Reason is a short execution-outcome explanation (not a quality judgement).
	Reason string `json:"reason,omitempty"`
}

// Artifact is a produced output (AC-RS1).
type Artifact struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
}

// ManifestEntry maps one declared output to its produced status (AC-RS3).
type ManifestEntry struct {
	Artifact string         `json:"artifact"`
	Status   ManifestStatus `json:"status"`
}

// Logs references the captured log surfaces (AC-RS1, AC-LC5).
type Logs struct {
	StdoutRef  string      `json:"stdout_ref,omitempty"`
	StderrRef  string      `json:"stderr_ref,omitempty"`
	TraceSpans []TraceSpan `json:"trace_spans,omitempty"`
}

// TraceSpan is one observation/trace event captured during execution.
type TraceSpan struct {
	Timestamp string `json:"ts,omitempty"`
	Type      string `json:"type,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// PolicyEvent is one allow|deny|escalation|violation event (AC-LC6).
type PolicyEvent struct {
	Kind PolicyEventKind `json:"kind"`
	Rule string          `json:"rule,omitempty"`
	Path string          `json:"path,omitempty"`
	Tool string          `json:"tool,omitempty"`
}

// ModelUsage is token/cost/model evidence for inference providers (AC-RS6).
type ModelUsage struct {
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Cost         string `json:"cost,omitempty"`
	ModelID      string `json:"model_id,omitempty"`
}

// RuntimeIdentity is the replay-identity anchor (AC-RS5). provider_id and
// version are always present; image_digest is present for container runtimes.
type RuntimeIdentity struct {
	ProviderID  string `json:"provider_id"`
	Version     string `json:"version"`
	ImageDigest string `json:"image_digest,omitempty"`
}

// ProviderError is the structured error on non-completed status (AC-RS4).
type ProviderError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
