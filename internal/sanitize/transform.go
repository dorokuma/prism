package sanitize

import (
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

// TransformRequestBody applies model remap, thinking field remap (for DeepSeek),
// and strips unsupported fields (per config) in a single JSON parse/marshal pass.
// Returns the original body unchanged if no transformation was needed.
func TransformRequestBody(body []byte, cfg *config.Config) []byte {
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

	// Step 2: Thinking field remap for DeepSeek models
	if util.IsDeepSeekModel(model) {
		if thinkRaw, ok := raw["thinking"]; ok && len(thinkRaw) > 0 && string(thinkRaw) != "null" {
			var thinking map[string]any
			if err := json.Unmarshal(thinkRaw, &thinking); err == nil {
				if level, ok := thinking["level"].(string); ok {
					mapped := util.MapThoughtLevel(level)
					if mapped != level {
						slog.Debug("thinking level remap", "model", model, "from", level, "to", mapped)
						thinking["level"] = mapped
						if b, err := json.Marshal(thinking); err == nil {
							raw["thinking"] = json.RawMessage(b)
							changed = true
						}
					}
				}
			}
		}
		if effortRaw, ok := raw["reasoning_effort"]; ok && len(effortRaw) > 0 && string(effortRaw) != "null" {
			var effort string
			if err := json.Unmarshal(effortRaw, &effort); err == nil {
				mapped := util.MapThoughtLevel(effort)
				if mapped != effort {
					slog.Debug("reasoning_effort remap", "model", model, "from", effort, "to", mapped)
					if b, err := json.Marshal(mapped); err == nil {
						raw["reasoning_effort"] = json.RawMessage(b)
						changed = true
					}
				}
			}
		}
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
