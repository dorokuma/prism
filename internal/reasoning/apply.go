package reasoning

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/dorokuma/prism/internal/util"
)

// Apply examines the reasoning-effort / thinking.level fields in raw and
// applies the mapping rules for the given upstream model using the default
// opencode schema. It returns true if the raw map was modified.
func Apply(raw map[string]json.RawMessage, model string) bool {
	return ApplyWithSchema(raw, model, SchemaOpencode)
}

// ApplyWithSchema is like Apply but selects the profile table via schema
// (see reasoning.Schema*). It returns true if the raw map was modified.
func ApplyWithSchema(raw map[string]json.RawMessage, model, schema string) bool {
	p := ProfileFor(model, schema)

	if p.Form == FormNone {
		return stripAll(raw)
	}

	// ── DeepSeek compatibility branch ──────────────────────────────────
	// Two-stage: first thinking.level, then reasoning_effort.
	// Each independently mapped via util.MapThoughtLevel.
	// No field deletion, no toggle manipulation.
	if p.DeepSeekCompat {
		return applyDeepSeekCompat(raw)
	}

	// ── Read abstract level ────────────────────────────────────────────
	level, hasLevel := readAbstractLevel(raw)
	if !hasLevel {
		return false // nothing to do
	}
	level = strings.ToLower(level)

	changed := false

	// ── off ────────────────────────────────────────────────────────────
	if level == "off" {
		switch p.Form {
		case FormEnum:
			if p.OffValue != "" {
				// ollama: off → write OffValue (keep field, do not touch toggle)
				for _, f := range p.EnumFields {
					setNestedField(raw, f, p.OffValue)
				}
				if p.ForceOn {
					slog.Warn("model does not support disabling thinking, forcing",
						"model", model, "forced", p.OffValue)
				}
			} else {
				// opencode: delete enum fields + toggle off (original behaviour)
				for _, f := range p.EnumFields {
					deleteNestedField(raw, f)
				}
				if p.ToggleField != "" {
					setNestedField(raw, p.ToggleField, p.ToggleOff)
				}
			}
			// Clean up the abstract entry field not consumed by this profile.
			if !sliceContains(p.EnumFields, "thinking.level") {
				deleteNestedField(raw, "thinking.level")
			}
			changed = true

		case FormToggle:
			changed = true
			if p.ForceOn {
				setNestedField(raw, p.ToggleField, p.ToggleOn)
				slog.Warn("model does not support disabling thinking, forcing on",
					"model", model)
			} else {
				setNestedField(raw, p.ToggleField, p.ToggleOff)
			}
			if p.StripEnumOnToggle {
				stripEnumEntryFields(raw)
			}

		case FormBudget:
			raw["enable_thinking"] = boolRaw(false)
			delete(raw, "thinking_budget")
			changed = true
		}

		// For budget, also strip entry fields for off
		if p.Form == FormBudget {
			stripEntryFields(raw)
		}

		return changed
	}

	// ── non-off ────────────────────────────────────────────────────────
	switch p.Form {
	case FormEnum:
		if val, ok := p.EffortMap[level]; ok {
			for _, f := range p.EnumFields {
				setNestedField(raw, f, val)
			}
			if p.ToggleField != "" {
				setNestedField(raw, p.ToggleField, p.ToggleOn)
			}
			changed = true
		}
		// Clean up entry fields not part of this profile's schema
		if !sliceContains(p.EnumFields, "reasoning_effort") {
			delete(raw, "reasoning_effort")
		}
		if !sliceContains(p.EnumFields, "thinking.level") {
			deleteNestedField(raw, "thinking.level")
		}

	case FormToggle:
		setNestedField(raw, p.ToggleField, p.ToggleOn)
		changed = true
		if p.StripEnumOnToggle {
			stripEnumEntryFields(raw)
		}

	case FormBudget:
		if budget, ok := p.BudgetMap[level]; ok {
			raw["enable_thinking"] = boolRaw(true)
			raw["thinking_budget"] = intRaw(budget)
			changed = true
		}
		stripEntryFields(raw)
	}

	return changed
}

// ── Helpers ──────────────────────────────────────────────────────────────

// applyDeepSeekCompat replicates the old Step 2 behaviour exactly:
// independently map thinking.level and reasoning_effort via util.MapThoughtLevel.
func applyDeepSeekCompat(raw map[string]json.RawMessage) bool {
	changed := false

	// thinking.level
	if thinkRaw, ok := raw["thinking"]; ok && len(thinkRaw) > 0 && string(thinkRaw) != "null" {
		var thinking map[string]any
		if err := json.Unmarshal(thinkRaw, &thinking); err == nil {
			if level, ok := thinking["level"].(string); ok {
				mapped := util.MapThoughtLevel(level)
				if mapped != level {
					thinking["level"] = mapped
					if b, err := json.Marshal(thinking); err == nil {
						raw["thinking"] = json.RawMessage(b)
						changed = true
					}
				}
			}
		}
	}

	// reasoning_effort
	if effortRaw, ok := raw["reasoning_effort"]; ok && len(effortRaw) > 0 && string(effortRaw) != "null" {
		var effort string
		if err := json.Unmarshal(effortRaw, &effort); err == nil {
			mapped := util.MapThoughtLevel(effort)
			if mapped != effort {
				if b, err := json.Marshal(mapped); err == nil {
					raw["reasoning_effort"] = json.RawMessage(b)
					changed = true
				}
			}
		}
	}

	return changed
}

// readAbstractLevel extracts the abstract reasoning level from the request body.
// Priority: reasoning_effort (top-level string) → thinking.level (nested).
func readAbstractLevel(raw map[string]json.RawMessage) (string, bool) {
	if level, ok := util.RawStringField(raw, "reasoning_effort"); ok && level != "" {
		return level, true
	}
	return getNestedString(raw, "thinking.level")
}

// setNestedField writes val into raw at the given dot-path.
// For a path without a dot (e.g. "reasoning_effort") it sets raw[path].
// For a nested path (e.g. "thinking.type") it unmarshals the parent object,
// sets the key, and marshals it back; if the parent does not exist it is created.
func setNestedField(raw map[string]json.RawMessage, path string, val any) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		b, _ := json.Marshal(val)
		raw[parts[0]] = json.RawMessage(b)
		return
	}

	parentKey := parts[0]
	childKey := parts[1]

	parentRaw, exists := raw[parentKey]
	var parent map[string]any
	if exists && len(parentRaw) > 0 && string(parentRaw) != "null" {
		_ = json.Unmarshal(parentRaw, &parent)
	}
	if parent == nil {
		parent = make(map[string]any)
	}
	parent[childKey] = val
	b, _ := json.Marshal(parent)
	raw[parentKey] = json.RawMessage(b)
}

// getNestedString reads a string value from raw at the given dot-path.
// Returns ("", false) if the path does not exist or the value is not a string.
func getNestedString(raw map[string]json.RawMessage, path string) (string, bool) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		return util.RawStringField(raw, parts[0])
	}

	parentRaw, ok := raw[parts[0]]
	if !ok || len(parentRaw) == 0 || string(parentRaw) == "null" {
		return "", false
	}
	var parent map[string]any
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		return "", false
	}
	v, ok := parent[parts[1]].(string)
	return v, ok
}

// deleteNestedField removes a key at the given dot-path from raw.
// For a top-level key it calls delete(raw, key). For a nested key it
// unmarshals the parent, deletes the child, and marshals back.
func deleteNestedField(raw map[string]json.RawMessage, path string) {
	parts := strings.SplitN(path, ".", 2)
	if len(parts) == 1 {
		delete(raw, parts[0])
		return
	}

	parentRaw, ok := raw[parts[0]]
	if !ok || len(parentRaw) == 0 || string(parentRaw) == "null" {
		return
	}
	var parent map[string]any
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		return
	}
	delete(parent, parts[1])
	if len(parent) == 0 {
		delete(raw, parts[0])
	} else {
		b, _ := json.Marshal(parent)
		raw[parts[0]] = json.RawMessage(b)
	}
}

// stripAll removes reasoning_effort and thinking from the top level of raw.
// Returns true if anything was removed.
func stripAll(raw map[string]json.RawMessage) bool {
	changed := false
	if _, ok := raw["reasoning_effort"]; ok {
		delete(raw, "reasoning_effort")
		changed = true
	}
	if _, ok := raw["thinking"]; ok {
		delete(raw, "thinking")
		changed = true
	}
	return changed
}

// stripEnumEntryFields removes the top-level reasoning_effort and the nested
// thinking.level from raw (used by FormToggle with StripEnumOnToggle).
func stripEnumEntryFields(raw map[string]json.RawMessage) {
	delete(raw, "reasoning_effort")
	deleteNestedField(raw, "thinking.level")
}

// stripEntryFields removes the top-level reasoning_effort and the whole
// thinking object from raw (used by FormBudget).
func stripEntryFields(raw map[string]json.RawMessage) {
	delete(raw, "reasoning_effort")
	delete(raw, "thinking")
}

func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func boolRaw(v bool) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

func intRaw(v int) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}
