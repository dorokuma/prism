package cache

import (
	"sort"
	"strings"

	"github.com/dorokuma/prism/internal/config"
)

// RoutingStatus classifies the aggregate provider resolution outcome for a
// model name (see ResolveProvider introduced by provider_routing: auto).
type RoutingStatus int

const (
	// RoutingResolved means a provider was determined (override, unique
	// candidate, or provider_priority).
	RoutingResolved RoutingStatus = iota
	// RoutingAmbiguous means the model is served by more than one provider
	// and no model_provider_overrides / provider_priority rule disambiguates
	// it. Callers must fail closed (never guess).
	RoutingAmbiguous
	// RoutingUnknown means no provider advertises the model (and no override
	// exists).
	RoutingUnknown
	// RoutingModelMissing means the model parameter is missing/empty.
	RoutingModelMissing
)

// ResolveProvider resolves the provider for a model under aggregate routing
// (provider_routing: auto). It returns (provider, ordered candidates, status):
//   - RoutingResolved: provider is the winner — model_provider_overrides >
//     unique registry candidate > provider_priority. The override branch may
//     return a provider that does not currently advertise the model
//     (force-route; the upstream then answers 404/400 honestly).
//   - RoutingAmbiguous: candidates holds >1 provider and no rule
//     disambiguated them; the caller must reject with a structured error and
//     never pick one silently.
//   - RoutingUnknown: no provider advertises the model.
//   - RoutingModelMissing: model name is empty.
//
// Callers must only consult it when the request carries no explicit
// X-Prism-Provider header — explicit pins bypass aggregate resolution.
func (mc *ModelCache) ResolveProvider(model string) (string, []string, RoutingStatus) {
	cfg := mc.snapshotConfig()
	if cfg == nil || cfg.ProviderRouting != "auto" {
		return "", nil, RoutingUnknown
	}
	if strings.TrimSpace(model) == "" {
		return "", nil, RoutingModelMissing
	}
	if p, ok := cfg.ModelProviderOverrides[model]; ok {
		return p, nil, RoutingResolved
	}
	candidates := mc.providerCandidates(cfg, model)
	if len(candidates) == 0 {
		return "", candidates, RoutingUnknown
	}
	if len(candidates) == 1 {
		return candidates[0], candidates, RoutingResolved
	}
	for _, p := range cfg.ProviderPriority {
		for _, c := range candidates {
			if p == c {
				return p, candidates, RoutingResolved
			}
		}
	}
	return "", candidates, RoutingAmbiguous
}

// providerCandidates returns the providers (in DeclaredProviderNames declaration
// order) whose model directory — cache snapshot or static config models —
// contains the id.
func (mc *ModelCache) providerCandidates(cfg *config.Config, model string) []string {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	var candidates []string
	for _, p := range cfg.DeclaredProviderNames() {
		if mc.providerHasModelLocked(cfg, p, model) {
			candidates = append(candidates, p)
		}
	}
	return candidates
}

func (mc *ModelCache) providerHasModelLocked(cfg *config.Config, provider, model string) bool {
	if pc := mc.caches[provider]; pc != nil {
		for _, m := range pc.Models {
			if m.ID == model {
				return true
			}
		}
	}
	for _, id := range cfg.StaticModels(provider) {
		if id == model {
			return true
		}
	}
	return false
}

// UnionModel is one aggregate-catalog entry: the winner provider's entry and
// its upstream metadata (Meta/HasMeta) for the model.
type UnionModel struct {
	Entry    ModelEntry
	Provider string
	Meta     ModelMeta
	HasMeta  bool
}

// UnionModels returns the aggregate model catalog for /v1/models under
// provider_routing: auto:
//   - one entry per model id (winner provider, same rules as ResolveProvider),
//     sorted by model id;
//   - the winner provider's upstream metadata attached when present;
//   - models whose winner cannot be resolved (RoutingAmbiguous) are EXCLUDED
//     from the catalog and returned in ambiguous (model -> candidate
//     providers) so the caller can surface them and never advertise a model
//     it cannot route.
//
// It never performs network I/O: the catalog is built from the caches and
// config static directories.
func (mc *ModelCache) UnionModels() ([]UnionModel, map[string][]string) {
	cfg := mc.snapshotConfig()
	if cfg == nil || cfg.ProviderRouting != "auto" {
		return nil, nil
	}
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	byModel := make(map[string][]string)
	for _, p := range cfg.DeclaredProviderNames() {
		if pc := mc.caches[p]; pc != nil {
			for _, m := range pc.Models {
				if m.ID == "" {
					continue
				}
				byModel[m.ID] = appendUnique(byModel[m.ID], p)
			}
		}
		for _, id := range cfg.StaticModels(p) {
			if id == "" {
				continue
			}
			byModel[id] = appendUnique(byModel[id], p)
		}
	}
	for model, targetProvider := range cfg.ModelProviderOverrides {
		if model == "" {
			continue
		}
		byModel[model] = appendUnique(byModel[model], targetProvider)
	}

	var out []UnionModel
	ambiguous := make(map[string][]string)
	for model, candidates := range byModel {
		provider, _, status := resolveProviderLocked(cfg, model, candidates)
		switch status {
		case RoutingResolved:
			out = append(out, mc.unionModelLocked(model, provider))
		case RoutingAmbiguous:
			ambiguous[model] = candidates
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Entry.ID < out[j].Entry.ID })
	return out, ambiguous
}

// resolveProviderLocked is ResolveProvider minus locking: callers hold mc.mu
// (or pass a config that cannot change under them).
func resolveProviderLocked(cfg *config.Config, model string, candidates []string) (string, []string, RoutingStatus) {
	if p, ok := cfg.ModelProviderOverrides[model]; ok {
		return p, candidates, RoutingResolved
	}
	if len(candidates) == 0 {
		return "", candidates, RoutingUnknown
	}
	if len(candidates) == 1 {
		return candidates[0], candidates, RoutingResolved
	}
	for _, p := range cfg.ProviderPriority {
		for _, c := range candidates {
			if p == c {
				return p, candidates, RoutingResolved
			}
		}
	}
	return "", candidates, RoutingAmbiguous
}

// unionModelLocked builds the winner entry and its upstream metadata;
// callers hold mc.mu.
func (mc *ModelCache) unionModelLocked(model, provider string) UnionModel {
	um := UnionModel{Provider: provider}
	if pc := mc.caches[provider]; pc != nil {
		for _, m := range pc.Models {
			if m.ID == model {
				um.Entry = m
				break
			}
		}
		if pc.Meta != nil {
			if meta, ok := pc.Meta[model]; ok {
				um.Meta = meta
				um.HasMeta = true
			}
		}
	}
	if um.Entry.ID == "" {
		// Static-directory entry (no cache row) or a forced override
		// pointing at a provider without an advertising row: emit the same
		// stable stub the model_remap AllModels branch uses so clients see a
		// real entry.
		um.Entry = ModelEntry{ID: model, Object: "model", Created: 1700000000, OwnedBy: "prism"}
	}
	return um
}

func appendUnique(list []string, item string) []string {
	for _, existing := range list {
		if existing == item {
			return list
		}
	}
	return append(list, item)
}
