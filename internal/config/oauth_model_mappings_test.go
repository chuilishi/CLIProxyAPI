package config

import "testing"

func TestSanitizeOAuthModelMappings_PreservesForkFlag(t *testing.T) {
	cfg := &Config{
		OAuthModelMappings: map[string][]ModelNameMapping{
			" CoDeX ": {
				{Name: " gpt-5 ", Alias: " g5 ", Fork: true},
				{Name: "gpt-6", Alias: "g6"},
			},
		},
	}

	cfg.SanitizeOAuthModelMappings()

	mappings := cfg.OAuthModelMappings["codex"]
	if len(mappings) != 2 {
		t.Fatalf("expected 2 sanitized mappings, got %d", len(mappings))
	}
	if mappings[0].Name != "gpt-5" || mappings[0].Alias != "g5" || !mappings[0].Fork {
		t.Fatalf("expected first mapping to be gpt-5->g5 fork=true, got name=%q alias=%q fork=%v", mappings[0].Name, mappings[0].Alias, mappings[0].Fork)
	}
	if mappings[1].Name != "gpt-6" || mappings[1].Alias != "g6" || mappings[1].Fork {
		t.Fatalf("expected second mapping to be gpt-6->g6 fork=false, got name=%q alias=%q fork=%v", mappings[1].Name, mappings[1].Alias, mappings[1].Fork)
	}
}

// TestSanitizeOAuthModelMappings_OneToMany tests that the same Name can map to multiple Aliases.
func TestSanitizeOAuthModelMappings_OneToMany(t *testing.T) {
	cfg := &Config{
		OAuthModelMappings: map[string][]ModelNameMapping{
			"antigravity": {
				{Name: "gemini-3-flash", Alias: "claude-sonnet-4-5-20250929"},
				{Name: "gemini-3-flash", Alias: "claude-haiku-4-5-20251001"},
				{Name: "gemini-3-flash", Alias: "claude-opus-4-5-20251101"},
			},
		},
	}

	cfg.SanitizeOAuthModelMappings()

	mappings := cfg.OAuthModelMappings["antigravity"]
	if len(mappings) != 3 {
		t.Fatalf("expected 3 mappings for one-to-many, got %d", len(mappings))
	}
	expectedAliases := []string{"claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001", "claude-opus-4-5-20251101"}
	for i, expected := range expectedAliases {
		if mappings[i].Alias != expected {
			t.Fatalf("expected mapping[%d].Alias=%q, got %q", i, expected, mappings[i].Alias)
		}
		if mappings[i].Name != "gemini-3-flash" {
			t.Fatalf("expected mapping[%d].Name=%q, got %q", i, "gemini-3-flash", mappings[i].Name)
		}
	}
}

// TestSanitizeOAuthModelMappings_DuplicateAliasRejected tests that duplicate aliases are rejected.
func TestSanitizeOAuthModelMappings_DuplicateAliasRejected(t *testing.T) {
	cfg := &Config{
		OAuthModelMappings: map[string][]ModelNameMapping{
			"antigravity": {
				{Name: "gemini-3-flash", Alias: "claude-sonnet"},
				{Name: "gemini-3-pro", Alias: "claude-sonnet"}, // duplicate alias, should be rejected
			},
		},
	}

	cfg.SanitizeOAuthModelMappings()

	mappings := cfg.OAuthModelMappings["antigravity"]
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping (duplicate alias rejected), got %d", len(mappings))
	}
	if mappings[0].Alias != "claude-sonnet" {
		t.Fatalf("expected first mapping alias=%q, got %q", "claude-sonnet", mappings[0].Alias)
	}
}
