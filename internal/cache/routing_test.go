package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/config"
)

// ---- Aggregate provider routing registry ----

func loadRoutingCfg(t *testing.T, yamlContent string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func cfgContent() string {
	return `
provider_routing: auto
provider_priority: [xai, gemini]
model_provider_overrides:
  grok-4.5: gemini
providers:
  xai:
    accounts:
      - name: SuperGrok
        key: k
        base_url: https://api.x.ai/v1
  gemini:
    models: [gemini-2.5-flash-static]
    accounts:
      - name: Gemini
        key: k
        base_url: https://cloudcode-pa.googleapis.com
`
}

func TestResolveProvider_UniqueCandidate(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai":    {Models: []ModelEntry{{ID: "grok-4.20"}}},
		"gemini": {Models: []ModelEntry{{ID: "gemini-2.5-pro"}}},
	}, cfg: cfg}

	p, candidates, status := mc.ResolveProvider("grok-4.20")
	if status != RoutingResolved || p != "xai" || len(candidates) != 1 {
		t.Fatalf("grok-4.20 = (%q, %v, %v), want (xai, [xai], resolved)", p, candidates, status)
	}
}

func TestResolveProvider_OverridesForceRoute(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "grok-4.5"}}},
	}, cfg: cfg}

	// Override wins even though xai advertises the model.
	p, _, status := mc.ResolveProvider("grok-4.5")
	if status != RoutingResolved || p != "gemini" {
		t.Fatalf("grok-4.5 = (%q, %v), want (gemini, resolved)", p, status)
	}
}

func TestResolveProvider_StaticDirectoryCandidate(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"gemini": {Models: []ModelEntry{{ID: "gemini-2.5-pro"}}},
	}, cfg: cfg}

	// Only the static directory advertises it; resolve must still win.
	p, _, status := mc.ResolveProvider("gemini-2.5-flash-static")
	if status != RoutingResolved || p != "gemini" {
		t.Fatalf("static model = (%q, %v), want (gemini, resolved)", p, status)
	}
}

func TestResolveProvider_PriorityDisambiguates(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai":    {Models: []ModelEntry{{ID: "shared-1"}}},
		"gemini": {Models: []ModelEntry{{ID: "shared-1"}}},
	}, cfg: cfg}

	p, candidates, status := mc.ResolveProvider("shared-1")
	if status != RoutingResolved || p != "xai" || len(candidates) != 2 {
		t.Fatalf("shared-1 = (%q, %v, %v), want (xai, [xai gemini], resolved)", p, candidates, status)
	}
}

func TestResolveProvider_AmbiguousFailsClosed(t *testing.T) {
	cfg := loadRoutingCfg(t, strings.Replace(cfgContent(), "provider_priority: [xai, gemini]\n", "", 1))
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai":    {Models: []ModelEntry{{ID: "shared-1"}}},
		"gemini": {Models: []ModelEntry{{ID: "shared-1"}}},
	}, cfg: cfg}

	p, candidates, status := mc.ResolveProvider("shared-1")
	if status != RoutingAmbiguous || p != "" || len(candidates) != 2 {
		t.Fatalf("shared-1 = (%q, %v, %v), want (\"\", [xai gemini], ambiguous)", p, candidates, status)
	}
}

func TestResolveProvider_Unknown(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "grok-4.20"}}},
	}, cfg: cfg}

	p, _, status := mc.ResolveProvider("does-not-exist")
	if status != RoutingUnknown || p != "" {
		t.Fatalf("unknown model = (%q, %v), want (\"\", unknown)", p, status)
	}
}

func TestResolveProvider_AggregateDisabled(t *testing.T) {
	cfg := loadRoutingCfg(t, strings.Replace(cfgContent(), "provider_routing: auto\n", "", 1))
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "grok-4.20"}}},
	}, cfg: cfg}

	if _, _, status := mc.ResolveProvider("grok-4.20"); status != RoutingUnknown {
		t.Fatalf("aggregate disabled must not resolve, got status %v", status)
	}
}

func TestUnionModels_MergesDedupsExcludesAmbiguous(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {
			Models: []ModelEntry{{ID: "grok-4.5", Object: "model", Created: 1, OwnedBy: "xai-up"}, {ID: "grok-4.20", Object: "model", Created: 2, OwnedBy: "xai-up"}, {ID: "shared-1", Object: "model", Created: 5, OwnedBy: "xai-up"}},
			Meta:   map[string]ModelMeta{"grok-4.5": {ContextWindow: intPtr(1000000)}},
		},
		"gemini": {
			Models: []ModelEntry{{ID: "gemini-2.5-pro", Object: "model", Created: 3, OwnedBy: "g-up"}, {ID: "shared-1", Object: "model", Created: 4, OwnedBy: "g-up"}},
		},
	}, cfg: cfg}

	// shared-1 collides across providers and provider_priority resolves it to
	// xai; grok-4.5 collides only implicitly (override force-routes it to
	// gemini); everything else is unique. No ambiguity is left, so the
	// ambiguous map must be empty and the union a 5-entry sorted catalog.
	out, ambiguous := mc.UnionModels()
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v, want none (priority/overrides resolve all)", ambiguous)
	}
	want := []string{"gemini-2.5-flash-static", "gemini-2.5-pro", "grok-4.20", "grok-4.5", "shared-1"}
	if got := idsOfUnion(out); len(got) != len(want) {
		t.Fatalf("union ids = %v, want %d entries", got, len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("union ids = %v, want %v", got, want)
			}
		}
	}
	// Static-directory-only model: winner gemini, stub entry.
	if out[0].Provider != "gemini" || out[0].Entry.Created != 1700000000 {
		t.Fatalf("static model entry = %+v, want gemini static stub", out[0])
	}
	// shared-1: winner xai (provider_priority beats gemini).
	if out[4].Provider != "xai" || out[4].Entry.OwnedBy != "xai-up" {
		t.Fatalf("shared-1 = %+v, want xai entry", out[4])
	}
	// grok-4.5: winner gemini (override) — metadata must come from the
	// WINNER provider (gemini has no upstream meta for it), NOT from the xai
	// cache that advertises context_window.
	if out[3].Provider != "gemini" || out[3].HasMeta {
		t.Fatalf("grok-4.5 = %+v, want gemini winner without xai metadata", out[3])
	}
}

func TestUnionModels_AmbiguousExcludedAndListed(t *testing.T) {
	cfg := loadRoutingCfg(t, strings.Replace(cfgContent(), "provider_priority: [xai, gemini]\n", "", 1))
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai":    {Models: []ModelEntry{{ID: "only-xai"}, {ID: "ambig-1"}}},
		"gemini": {Models: []ModelEntry{{ID: "no-priority-model"}, {ID: "ambig-1"}}},
	}, cfg: cfg}

	out, ambiguous := mc.UnionModels()
	if got := ambiguous["ambig-1"]; len(got) != 2 || got[0] != "xai" || got[1] != "gemini" {
		t.Fatalf("ambiguous[ambig-1] = %v, want [xai gemini]", got)
	}
	for _, um := range out {
		if um.Entry.ID == "ambig-1" {
			t.Fatalf("ambiguous model must be excluded from union, got %+v", um)
		}
	}
	if len(out) != 4 {
		// only-xai + no-priority-model + the static-directory model
		// (gemini-2.5-flash-static) + the override model (grok-4.5) from cfgContent.
		t.Fatalf("union size = %d, want 4 (only-xai, no-priority-model, static, grok-4.5 override)", len(out))
	}
}

func TestResolveProvider_EmptyModel(t *testing.T) {
	cfg := loadRoutingCfg(t, cfgContent())
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "grok-4.20"}}},
	}, cfg: cfg}

	for _, m := range []string{"", " ", "   ", "\t\n"} {
		p, candidates, status := mc.ResolveProvider(m)
		if status != RoutingModelMissing || p != "" || len(candidates) != 0 {
			t.Fatalf("model %q = (%q, %v, %v), want (\"\", [], RoutingModelMissing)", m, p, candidates, status)
		}
	}
}

func TestUnionModels_PureOverridesAppearsInUnion(t *testing.T) {
	content := `
provider_routing: auto
model_provider_overrides:
  pure-override-model: xai
providers:
  xai:
    accounts:
      - name: a
        key: k
        base_url: https://api.x.ai/v1
`
	cfg := loadRoutingCfg(t, content)
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "cached-model"}}},
	}, cfg: cfg}

	out, ambiguous := mc.UnionModels()
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v, want none", ambiguous)
	}
	ids := idsOfUnion(out)
	found := false
	for _, id := range ids {
		if id == "pure-override-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pure-override-model not found in union models: %v", ids)
	}
}

func TestUnionModels_PureStaticProviderWithoutAccounts(t *testing.T) {
	content := `
provider_routing: auto
provider_priority: [gemini, xai]
providers:
  xai:
    accounts:
      - name: a
        key: k
        base_url: https://api.x.ai/v1
  gemini:
    models: [gemini-static-pure]
`
	cfg := loadRoutingCfg(t, content)
	mc := &ModelCache{caches: map[string]*providerCache{
		"xai": {Models: []ModelEntry{{ID: "xai-cached"}}},
	}, cfg: cfg}

	out, ambiguous := mc.UnionModels()
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v, want none", ambiguous)
	}
	ids := idsOfUnion(out)
	found := false
	for _, id := range ids {
		if id == "gemini-static-pure" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("gemini-static-pure should appear in union models, got %v", ids)
	}

	p, candidates, status := mc.ResolveProvider("gemini-static-pure")
	if status != RoutingResolved || p != "gemini" || len(candidates) != 1 {
		t.Fatalf("ResolveProvider(gemini-static-pure) = (%q, %v, %v), want (gemini, [gemini], resolved)", p, candidates, status)
	}
}

func idsOfUnion(us []UnionModel) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Entry.ID)
	}
	return out
}

