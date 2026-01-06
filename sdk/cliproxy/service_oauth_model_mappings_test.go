package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestApplyOAuthModelMappings_Rename(t *testing.T) {
	cfg := &config.Config{
		OAuthModelMappings: map[string][]config.ModelNameMapping{
			"codex": {
				{Name: "gpt-5", Alias: "g5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelMappings(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "g5" {
		t.Fatalf("expected model id %q, got %q", "g5", out[0].ID)
	}
	if out[0].Name != "models/g5" {
		t.Fatalf("expected model name %q, got %q", "models/g5", out[0].Name)
	}
}

func TestApplyOAuthModelMappings_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelMappings: map[string][]config.ModelNameMapping{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelMappings(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
}

// TestApplyOAuthModelMappings_OneToMany tests that a single backend model can be
// mapped to multiple aliases (one-to-many mapping support).
func TestApplyOAuthModelMappings_OneToMany(t *testing.T) {
	cfg := &config.Config{
		OAuthModelMappings: map[string][]config.ModelNameMapping{
			"antigravity": {
				{Name: "gemini-3-flash", Alias: "claude-sonnet-4-5-20250929"},
				{Name: "gemini-3-flash", Alias: "claude-haiku-4-5-20251001"},
				{Name: "gemini-3-flash", Alias: "claude-opus-4-5-20251101"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gemini-3-flash", Name: "models/gemini-3-flash"},
	}

	out := applyOAuthModelMappings(cfg, "antigravity", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models for one-to-many mapping, got %d", len(out))
	}

	expectedAliases := []string{"claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001", "claude-opus-4-5-20251101"}
	for i, expected := range expectedAliases {
		if out[i].ID != expected {
			t.Fatalf("expected model[%d] id %q, got %q", i, expected, out[i].ID)
		}
	}
}

// TestApplyOAuthModelMappings_OneToManyWithFork tests one-to-many mapping with fork enabled.
func TestApplyOAuthModelMappings_OneToManyWithFork(t *testing.T) {
	cfg := &config.Config{
		OAuthModelMappings: map[string][]config.ModelNameMapping{
			"antigravity": {
				{Name: "gemini-3-flash", Alias: "claude-sonnet-4-5-20250929", Fork: true},
				{Name: "gemini-3-flash", Alias: "claude-haiku-4-5-20251001", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gemini-3-flash", Name: "models/gemini-3-flash"},
	}

	out := applyOAuthModelMappings(cfg, "antigravity", "oauth", models)
	// With fork: original + 2 aliases = 3 models
	if len(out) != 3 {
		t.Fatalf("expected 3 models (1 original + 2 aliases), got %d", len(out))
	}

	// First should be the original model
	if out[0].ID != "gemini-3-flash" {
		t.Fatalf("expected first model id %q, got %q", "gemini-3-flash", out[0].ID)
	}
	// Then the aliases
	if out[1].ID != "claude-sonnet-4-5-20250929" {
		t.Fatalf("expected second model id %q, got %q", "claude-sonnet-4-5-20250929", out[1].ID)
	}
	if out[2].ID != "claude-haiku-4-5-20251001" {
		t.Fatalf("expected third model id %q, got %q", "claude-haiku-4-5-20251001", out[2].ID)
	}
}
