package sanitize

import (
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/reasoning"
	"github.com/dorokuma/prism/internal/util"
)

// TransformRequestBody applies model remap, reasoning effort / thinking mapping
// (per upstream model via internal/reasoning), and strips unsupported fields
// (per config) in a single JSON parse/marshal pass.
// Returns the original body unchanged if no transformation was needed.
func TransformRequestBody(body []byte, cfg *config.Config) []byte {
	return TransformRequestBodyForProvider(body, cfg, "")
}

// TransformRequestBodyForProvider is like TransformRequestBody but selects the
// effort schema via the upstream provider (read from the X-Prism-Provider
// header by the caller). The provider is used to choose between the opencode
// and ollama profile tables. An empty provider selects the opencode table.
func TransformRequestBodyForProvider(body []byte, cfg *config.Config, provider string) []byte {
	if cfg == nil {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	changed := false

	// Step 1: Model name remap
	model, ok := util.RawStringField(raw, "model")
	if ok && model != "" {
		remapped := cfg.RemapModel(model)
		if remapped != model {
			rawBytes, _ := json.Marshal(remapped)
			raw["model"] = json.RawMessage(rawBytes)
			changed = true
			slog.Debug("model remap", "from", model, "to", remapped)
			model = remapped // use remapped name for downstream steps
		}
	}

	// Step 2: Reasoning effort / thinking mapping for all models
	schema := cfg.EffortSchema(provider)
	if reasoning.ApplyWithSchema(raw, model, schema) {
		changed = true
	}

	// Step 3: Strip unsupported fields per tier config
	// Aggregate StripFields across all tiers whose upstream matches the model.
	if len(cfg.StripFields) > 0 && model != "" {
		var matchedTiers []string
		seenFields := make(map[string]bool)
		var mergedFields []string
		for t, upstream := range cfg.ModelTiers {
			if upstream == model {
				matchedTiers = append(matchedTiers, t)
				if fields, ok := cfg.StripFields[t]; ok {
					for _, f := range fields {
						if !seenFields[f] {
							seenFields[f] = true
							mergedFields = append(mergedFields, f)
						}
					}
				}
			}
		}
		if len(mergedFields) > 0 {
			sort.Strings(matchedTiers)
			for _, field := range mergedFields {
				if _, exists := raw[field]; exists {
					delete(raw, field)
					changed = true
					slog.Debug("stripped field", "field", field, "model", model, "tiers", matchedTiers)
				}
			}
		}
	}

	if !changed {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}
