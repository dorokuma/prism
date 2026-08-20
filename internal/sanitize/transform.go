package sanitize

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

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
	modelForReasoning := model
	// clinepass serves vendor-prefixed ids (cline-pass/deepseek-v4-flash).
	// ProfileFor matches the prefix on the whole string, so the slash form
	// would miss the deepseek- table and strip thinking. Only clinepass is
	// rewritten this way; every other provider keeps the original name.
	if provider == "clinepass" {
		if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
			modelForReasoning = model[i+1:]
		}
	}
	if reasoning.ApplyWithSchema(raw, modelForReasoning, schema) {
		changed = true
	}

	// Step 2.5: Message role normalization (developer → system) — ollama only.
	//
	// Ollama silently drops the non-standard "developer" role, which would
	// cause the SYSTEM.md / AGENTS.md context to be lost. Other upstreams
	// (e.g. opencode-go) either support developer natively or pi already
	// sends role:system, so we gate this step to the ollama schema only.
	// The /v1/responses path already normalizes via
	// NormalizeMessagesForChatAPI, but that helper also flattens multimodal
	// content into plain text and merges consecutive system turns. For the
	// chat/completions path we must preserve images and every other field
	// exactly as sent, so we perform only the minimal developer→system role
	// rewrite here (no flatten, no merge).
	if schema == reasoning.SchemaOllama {
		if msgsRaw, ok := raw["messages"]; ok && len(msgsRaw) > 0 {
			var messages []map[string]any
			if err := json.Unmarshal(msgsRaw, &messages); err == nil && len(messages) > 0 {
				if normalizeMessageRoles(messages) {
					rawBytes, _ := json.Marshal(messages)
					raw["messages"] = json.RawMessage(rawBytes)
					changed = true
					slog.Debug("normalized message roles (developer → system)")
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

// normalizeMessageRoles rewrites every message whose role is "developer" to
// "system", leaving all other fields (including multimodal content) untouched.
// It returns true if any role was rewritten.
//
// Unlike NormalizeMessagesForChatAPI this does NOT flatten multimodal content
// into plain text and does NOT merge consecutive system turns, because the
// chat/completions path must forward images and other fields verbatim. The
// sole goal here is to prevent upstreams from dropping the non-standard
// "developer" role (and thus the SYSTEM.md / AGENTS.md context).
func normalizeMessageRoles(messages []map[string]any) bool {
	changed := false
	for _, m := range messages {
		if role, ok := m["role"].(string); ok && role == "developer" {
			m["role"] = "system"
			changed = true
		}
	}
	return changed
}
