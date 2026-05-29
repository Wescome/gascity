package config

import (
	"reflect"
	"sort"
	"testing"
)

// The singular [provider.*] blocks parse into City.Provider with the harness
// fields (IS-GC-RUNTIME-PROVIDER-CONTRACT AC-REG1/AC-REG4/AC-REG5).
func TestParse_HarnessProviderBlocks(t *testing.T) {
	data := []byte(`
[workspace]
name = "factory"

[provider.pi-rpc]
url = "https://ff-pipeline.koales.workers.dev"
harness_slots = ["E", "T", "C", "S", "L", "V", "G", "P"]
capability_keys = ["ai_reasoning", "model_routing", "workspace_write_scope", "contract_evaluation", "tool_capability_probe", "session_archive"]

[provider.cloudflare-sandbox]
url = "https://gascity-cloudflare-control-worker.koales.workers.dev"
harness_slots = ["E", "T", "C", "S", "L", "V", "G", "P"]
capability_keys = ["command_exec", "file_materialize", "workspace_init", "dependency_prep", "backup_restore"]
`)
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	pi, ok := cfg.Provider["pi-rpc"]
	if !ok {
		t.Fatalf("expected [provider.pi-rpc] parsed")
	}
	if pi.URL != "https://ff-pipeline.koales.workers.dev" {
		t.Fatalf("pi-rpc url = %q", pi.URL)
	}
	if len(pi.HarnessSlots) != 8 {
		t.Fatalf("pi-rpc harness_slots = %v", pi.HarnessSlots)
	}
	wantKeys := []string{"ai_reasoning", "contract_evaluation", "model_routing", "session_archive", "tool_capability_probe", "workspace_write_scope"}
	gotKeys := append([]string(nil), pi.CapabilityKeys...)
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("pi-rpc capability_keys = %v, want %v", gotKeys, wantKeys)
	}

	sandbox, ok := cfg.Provider["cloudflare-sandbox"]
	if !ok {
		t.Fatalf("expected [provider.cloudflare-sandbox] parsed")
	}
	for _, k := range sandbox.CapabilityKeys {
		if k == "ai_reasoning" {
			t.Fatalf("cloudflare-sandbox MUST NOT declare ai_reasoning (AC-HT3)")
		}
	}
}

// HarnessProviderBlocks returns the singular [provider.*] blocks as neutral
// descriptors for the registry builder.
func TestHarnessProviderBlocks(t *testing.T) {
	cfg := &City{
		Provider: map[string]ProviderSpec{
			"pi-rpc": {
				URL:            "https://ff-pipeline.koales.workers.dev",
				HarnessSlots:   []string{"E", "T", "C", "S", "L", "V", "G", "P"},
				CapabilityKeys: []string{"ai_reasoning"},
			},
		},
	}
	blocks := cfg.HarnessProviderBlocks()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].ID != "pi-rpc" || blocks[0].URL != "https://ff-pipeline.koales.workers.dev" {
		t.Fatalf("unexpected block: %+v", blocks[0])
	}
	if len(blocks[0].HarnessSlots) != 8 {
		t.Fatalf("expected 8 slots, got %v", blocks[0].HarnessSlots)
	}
}

// The bearer token resolves from the literal Token when set; otherwise it is
// read from the named TokenEnv environment variable. This keeps the operator
// control secret out of city.toml: the TOML only names the env var
// (e.g. OPERATOR_CONTROL_TOKEN), and the operator supplies the value via the
// harness container environment. Gap 2 closure for the pi-rpc provider.
func TestHarnessProviderBlocks_TokenResolution(t *testing.T) {
	t.Run("literal token wins over env var", func(t *testing.T) {
		t.Setenv("FF_TEST_OPERATOR_TOKEN", "from-env")
		cfg := &City{
			Provider: map[string]ProviderSpec{
				"pi-rpc": {URL: "https://ff-pipeline.test", Token: "literal", TokenEnv: "FF_TEST_OPERATOR_TOKEN"},
			},
		}
		blocks := cfg.HarnessProviderBlocks()
		if blocks[0].Token != "literal" {
			t.Fatalf("Token = %q, want %q (literal must win)", blocks[0].Token, "literal")
		}
		if blocks[0].TokenEnv != "FF_TEST_OPERATOR_TOKEN" {
			t.Fatalf("TokenEnv = %q, want %q", blocks[0].TokenEnv, "FF_TEST_OPERATOR_TOKEN")
		}
	})

	t.Run("empty token resolves from env var", func(t *testing.T) {
		t.Setenv("FF_TEST_OPERATOR_TOKEN", "from-env")
		cfg := &City{
			Provider: map[string]ProviderSpec{
				"pi-rpc": {URL: "https://ff-pipeline.test", TokenEnv: "FF_TEST_OPERATOR_TOKEN"},
			},
		}
		blocks := cfg.HarnessProviderBlocks()
		if blocks[0].Token != "from-env" {
			t.Fatalf("Token = %q, want %q (resolved from TokenEnv)", blocks[0].Token, "from-env")
		}
	})

	t.Run("empty token and unset env yields empty token", func(t *testing.T) {
		cfg := &City{
			Provider: map[string]ProviderSpec{
				"pi-rpc": {URL: "https://ff-pipeline.test", TokenEnv: "FF_TEST_OPERATOR_TOKEN_UNSET"},
			},
		}
		blocks := cfg.HarnessProviderBlocks()
		if blocks[0].Token != "" {
			t.Fatalf("Token = %q, want empty (env var unset)", blocks[0].Token)
		}
		if blocks[0].TokenEnv != "FF_TEST_OPERATOR_TOKEN_UNSET" {
			t.Fatalf("TokenEnv = %q, want %q", blocks[0].TokenEnv, "FF_TEST_OPERATOR_TOKEN_UNSET")
		}
	})

	t.Run("no token fields yields empty token", func(t *testing.T) {
		cfg := &City{
			Provider: map[string]ProviderSpec{
				"pi-rpc": {URL: "https://ff-pipeline.test"},
			},
		}
		blocks := cfg.HarnessProviderBlocks()
		if blocks[0].Token != "" || blocks[0].TokenEnv != "" {
			t.Fatalf("Token=%q TokenEnv=%q, want both empty", blocks[0].Token, blocks[0].TokenEnv)
		}
	})
}

// Blocks are returned in a stable (id-sorted) order so registry preference and
// replay are deterministic (AC-REG2 determinism).
func TestHarnessProviderBlocks_DeterministicOrder(t *testing.T) {
	cfg := &City{
		Provider: map[string]ProviderSpec{
			"pi-rpc":             {URL: "a"},
			"cloudflare-sandbox": {URL: "b"},
		},
	}
	blocks := cfg.HarnessProviderBlocks()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].ID != "cloudflare-sandbox" || blocks[1].ID != "pi-rpc" {
		t.Fatalf("expected id-sorted order, got %q,%q", blocks[0].ID, blocks[1].ID)
	}
}
