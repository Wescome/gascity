package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// stepToBead must stamp a step's RuntimeRequirements onto the bead metadata as
// a comma-separated list under gc.runtime_requirements so the control-dispatcher
// can drive fail-closed harness provider selection.
func TestStepToBeadStampsRuntimeRequirements(t *testing.T) {
	step := formula.RecipeStep{
		ID:                  "demo.implement",
		Title:               "Implement change",
		Type:                "task",
		RuntimeRequirements: []string{"ai_reasoning", "model_routing", "workspace_write_scope"},
	}

	b := stepToBead(step, nil, nil)

	got := b.Metadata[RuntimeRequirementsMetadataKey]
	want := "ai_reasoning,model_routing,workspace_write_scope"
	if got != want {
		t.Fatalf("%s = %q, want %q", RuntimeRequirementsMetadataKey, got, want)
	}
}

// A step without RuntimeRequirements must not stamp the metadata key, so steps
// without requirements continue through the existing (non-harness) path.
func TestStepToBeadOmitsRuntimeRequirementsWhenEmpty(t *testing.T) {
	step := formula.RecipeStep{
		ID:    "demo.plain",
		Title: "No requirements",
		Type:  "task",
	}

	b := stepToBead(step, nil, nil)

	if _, ok := b.Metadata[RuntimeRequirementsMetadataKey]; ok {
		t.Fatalf("expected %s to be absent for a step with no requirements, metadata=%v", RuntimeRequirementsMetadataKey, b.Metadata)
	}
}
