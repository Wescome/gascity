package formula

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A step's runtime_requirements key set must survive TOML parsing. The Step
// struct has a custom UnmarshalTOML that routes through stepTOMLAlias.toStep(),
// so a new top-level TOML field is silently dropped unless the alias carries it.
func TestStepUnmarshalTOMLCarriesRuntimeRequirements(t *testing.T) {
	parser := NewParser()
	data := []byte(`
formula = "harness-demo"
description = "demo"
version = 1

[[steps]]
id = "implement"
title = "Implement change"
runtime_requirements = ["ai_reasoning", "model_routing", "workspace_write_scope"]
`)
	f, err := parser.ParseTOML(data)
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(f.Steps))
	}
	got := f.Steps[0].RuntimeRequirements
	want := []string{"ai_reasoning", "model_routing", "workspace_write_scope"}
	if len(got) != len(want) {
		t.Fatalf("RuntimeRequirements = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RuntimeRequirements[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// flattenSteps must propagate RuntimeRequirements from formula.Step onto the
// compiled RecipeStep so the harness dispatch path can read it downstream.
func TestCompilePropagatesRuntimeRequirementsToRecipeStep(t *testing.T) {
	dir := t.TempDir()
	content := `
formula = "harness-demo"
description = "demo"
version = 1

[[steps]]
id = "plain"
title = "No requirements"

[[steps]]
id = "implement"
title = "Implement change"
runtime_requirements = ["ai_reasoning", "command_exec"]
`
	if err := os.WriteFile(filepath.Join(dir, "harness-demo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	recipe, err := Compile(context.Background(), "harness-demo", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	var plain, implement *RecipeStep
	for i := range recipe.Steps {
		switch recipe.Steps[i].ID {
		case "harness-demo.plain":
			plain = &recipe.Steps[i]
		case "harness-demo.implement":
			implement = &recipe.Steps[i]
		}
	}
	if plain == nil || implement == nil {
		t.Fatalf("expected both steps in recipe, got %+v", recipe.Steps)
	}
	if len(plain.RuntimeRequirements) != 0 {
		t.Errorf("plain step RuntimeRequirements = %v, want empty", plain.RuntimeRequirements)
	}
	want := []string{"ai_reasoning", "command_exec"}
	if len(implement.RuntimeRequirements) != len(want) {
		t.Fatalf("implement RuntimeRequirements = %v, want %v", implement.RuntimeRequirements, want)
	}
	for i := range want {
		if implement.RuntimeRequirements[i] != want[i] {
			t.Errorf("implement RuntimeRequirements[%d] = %q, want %q", i, implement.RuntimeRequirements[i], want[i])
		}
	}
}
