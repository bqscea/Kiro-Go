package config

import (
	"os"
	"path/filepath"
	"testing"
)

// setupTestCfg installs an isolated cfg for unit tests and returns a teardown.
// The on-disk path is required by Save(), so we point it at a temp file.
func setupTestCfg(t *testing.T, c *Config) func() {
	t.Helper()
	cfgLock.Lock()
	prevCfg := cfg
	prevPath := cfgPath
	cfg = c
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	cfgLock.Unlock()
	return func() {
		cfgLock.Lock()
		cfg = prevCfg
		cfgPath = prevPath
		cfgLock.Unlock()
		_ = os.Remove(cfgPath)
	}
}

func TestResolveModelAliasFor(t *testing.T) {
	aliases := []ModelAlias{
		// Global alias for gpt-4o (no KeyIDs).
		{From: "gpt-4o", To: "claude-sonnet-4-5", Enabled: true},
		// Per-key alias bound to "key-A" — must win when key-A presents itself.
		{From: "gpt-4o", To: "claude-sonnet-4-6", Enabled: true, KeyIDs: []string{"key-A"}},
		// Per-key alias bound to multiple keys.
		{From: "claude-3-opus", To: "claude-opus-4", Enabled: true, KeyIDs: []string{"key-A", "key-B"}},
		// Disabled alias must never match.
		{From: "gpt-3.5", To: "claude-haiku", Enabled: false},
	}

	teardown := setupTestCfg(t, &Config{ModelAliases: aliases})
	defer teardown()

	cases := []struct {
		name  string
		model string
		keyID string
		want  string
	}{
		{"key-bound hit beats global", "gpt-4o", "key-A", "claude-sonnet-4-6"},
		{"unmatched key falls back to global", "gpt-4o", "key-Z", "claude-sonnet-4-5"},
		{"empty keyID falls back to global", "gpt-4o", "", "claude-sonnet-4-5"},
		{"key-bound only, no global -> bound match", "claude-3-opus", "key-B", "claude-opus-4"},
		{"key-bound only, no global, unrelated key -> unchanged", "claude-3-opus", "key-Z", "claude-3-opus"},
		{"key-bound only, no global, empty key -> unchanged", "claude-3-opus", "", "claude-3-opus"},
		{"no alias defined -> unchanged", "mystery-model", "key-A", "mystery-model"},
		{"disabled alias ignored", "gpt-3.5", "key-A", "gpt-3.5"},
		{"case insensitive From match honors key binding", "GPT-4O", "key-A", "claude-sonnet-4-6"},
		{"empty model returns input", "", "key-A", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveModelAliasFor(tc.model, tc.keyID)
			if got != tc.want {
				t.Fatalf("ResolveModelAliasFor(%q,%q) = %q; want %q", tc.model, tc.keyID, got, tc.want)
			}
		})
	}

	// Backward-compat wrapper resolves to the global alias.
	if got := ResolveModelAlias("gpt-4o"); got != "claude-sonnet-4-5" {
		t.Fatalf("ResolveModelAlias wrapper got %q; want claude-sonnet-4-5", got)
	}
}

func TestGetModelAliasesDeepCopiesKeyIDs(t *testing.T) {
	teardown := setupTestCfg(t, &Config{
		ModelAliases: []ModelAlias{
			{From: "a", To: "b", Enabled: true, KeyIDs: []string{"k1", "k2"}},
		},
	})
	defer teardown()

	out := GetModelAliases()
	if len(out) != 1 || len(out[0].KeyIDs) != 2 {
		t.Fatalf("unexpected snapshot: %+v", out)
	}
	// Mutate the returned slice; original config must remain intact.
	out[0].KeyIDs[0] = "MUTATED"

	again := GetModelAliases()
	if again[0].KeyIDs[0] != "k1" {
		t.Fatalf("GetModelAliases leaked underlying KeyIDs slice; got %q", again[0].KeyIDs[0])
	}
}
