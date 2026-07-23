package sanitize_test

import (
	"encoding/json"
	"testing"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

func assertBodyUnchanged(t *testing.T, got, body []byte) {
	t.Helper()
	if string(got) != string(body) {
		t.Errorf("body changed: got %s, want %s", got, body)
	}
	if len(got) > 0 && len(body) > 0 && &got[0] != &body[0] {
		t.Errorf("body slice identity changed: expected same underlying array")
	}
}

func TestTransformRequestBody_NilCfg(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, nil)
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_StripGLM(t *testing.T) {
	cfg := &config.Config{
		ModelRemap: map[string]string{"glm-5.2": "glm-standard"},
		ModelTiers: map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{
			"glm-standard": {"prompt_cache_retention"},
		},
	}

	body := []byte(`{"model":"glm-5.2","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	got := sanitize.TransformRequestBody(body, cfg)

	// Should be different from input (field stripped)
	if string(got) == string(body) {
		t.Fatal("TransformRequestBody returned same body, expected stripped body")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// prompt_cache_retention should be gone
	if _, ok := raw["prompt_cache_retention"]; ok {
		t.Error("prompt_cache_retention was not stripped")
	}

	// Other fields should remain
	if _, ok := raw["messages"]; !ok {
		t.Error("messages field missing after strip")
	}
	if _, ok := raw["temperature"]; !ok {
		t.Error("temperature field missing after strip")
	}

	// Model should also be remapped
	model, ok := util.RawStringField(raw, "model")
	if !ok {
		t.Fatal("model field missing after transform")
	}
	if model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2", model)
	}
}

func TestTransformRequestBody_DeepSeekNoThinkingNoStrip(t *testing.T) {
	cfg := &config.Config{
		ModelRemap: map[string]string{"deepseek-v4-pro": "frontier"},
		ModelTiers: map[string]string{"frontier": "deepseek-v4-pro"},
		StripFields: map[string][]string{
			"glm-standard": {"prompt_cache_retention"},
		},
	}

	body := []byte(`{"model":"deepseek-v4-pro","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	// This is a DeepSeek model but has no thinking/reasoning_effort fields and
	// no matching tier with strip_fields, so the body should be returned as-is.
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_GLMNoStripField(t *testing.T) {
	cfg := &config.Config{
		ModelRemap:  map[string]string{"glm-5.2": "glm-standard"},
		ModelTiers:  map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}

	// Body without prompt_cache_retention, model should still be remapped
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	model, _ := util.RawStringField(raw, "model")
	if model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2", model)
	}
	if _, ok := raw["prompt_cache_retention"]; ok {
		t.Error("prompt_cache_retention unexpectedly present")
	}
}

func TestTransformRequestBody_StripMultipleFields(t *testing.T) {
	cfg := &config.Config{
		ModelRemap: map[string]string{"glm-5.2": "glm-standard"},
		ModelTiers: map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{
			"glm-standard": {"prompt_cache_retention", "bad_field_1", "bad_field_2"},
		},
	}

	body := []byte(`{"model":"glm-5.2","prompt_cache_retention":5,"bad_field_1":"x","bad_field_2":"y","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if _, ok := raw["prompt_cache_retention"]; ok {
		t.Error("prompt_cache_retention was not stripped")
	}
	if _, ok := raw["bad_field_1"]; ok {
		t.Error("bad_field_1 was not stripped")
	}
	if _, ok := raw["bad_field_2"]; ok {
		t.Error("bad_field_2 was not stripped")
	}
	if _, ok := raw["temperature"]; !ok {
		t.Error("temperature field missing after strip")
	}
	if _, ok := raw["messages"]; !ok {
		t.Error("messages field missing after strip")
	}
}

func TestTransformRequestBody_ModelRemapAndStrip(t *testing.T) {
	// Virtual model glm-5.2 → tier glm-standard → upstream glm-5.2
	// Then strip prompt_cache_retention for glm-standard
	cfg := &config.Config{
		ModelRemap:  map[string]string{"glm-5.2": "glm-standard"},
		ModelTiers:  map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}

	body := []byte(`{"model":"glm-5.2","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	model, _ := util.RawStringField(raw, "model")
	if model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2 (remapped glm-5.2→glm-standard→glm-5.2)", model)
	}
	if _, ok := raw["prompt_cache_retention"]; ok {
		t.Error("prompt_cache_retention was not stripped")
	}
}

func TestTransformRequestBody_NoStripForNonMatchingTier(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"gpt-5.5": "frontier"},
		ModelTiers:        map[string]string{"frontier": "deepseek-v4-pro"},
		StripFields:       map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}

	body := []byte(`{"model":"gpt-5.5","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	// Model gpt-5.5 → frontier → deepseek-v4-pro; no strip_fields for frontier tier
	// But model remap did happen (gpt-5.5 → deepseek-v4-pro), so body should have changed
	if string(got) == string(body) {
		t.Fatal("TransformRequestBody should have remapped model, but returned same body")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	model, _ := util.RawStringField(raw, "model")
	if model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want deepseek-v4-pro", model)
	}

	// prompt_cache_retention should remain (no strip for this tier)
	if _, ok := raw["prompt_cache_retention"]; !ok {
		t.Error("prompt_cache_retention was stripped but shouldn't have been")
	}
}

func TestTransformRequestBody_InvalidJSON(t *testing.T) {
	cfg := &config.Config{
		StripFields: map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}
	body := []byte(`{invalid json}`)
	got := sanitize.TransformRequestBody(body, cfg)
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_DeepSeekThinkingRemap(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"gpt-5.5": "frontier"},
		ModelTiers:        map[string]string{"frontier": "deepseek-v4-pro"},
	}

	// DeepSeek model with thinking.level = low, should be remapped to high
	body := []byte(`{"model":"gpt-5.5","thinking":{"level":"low"},"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	if string(got) == string(body) {
		t.Fatal("TransformRequestBody should have remapped thinking level, but returned same body")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	model, _ := util.RawStringField(raw, "model")
	if model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want deepseek-v4-pro", model)
	}

	// Check thinking.level was remapped
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	level, _ := thinking["level"].(string)
	if level != "high" {
		t.Errorf("thinking.level = %q, want high", level)
	}
}

func TestTransformRequestBody_EmptyModel(t *testing.T) {
	cfg := &config.Config{
		ModelTiers:  map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}
	// Empty model → no remap, no strip
	body := []byte(`{"model":"","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_NoModelKey(t *testing.T) {
	cfg := &config.Config{
		ModelTiers:  map[string]string{"glm-standard": "glm-5.2"},
		StripFields: map[string][]string{"glm-standard": {"prompt_cache_retention"}},
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_DeepSeekReasoningEffortRemap(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"gpt-5.5": "frontier"},
		ModelTiers:        map[string]string{"frontier": "deepseek-v4-pro"},
	}

	tests := []struct {
		name       string
		inputLevel string
		wantLevel  string
	}{
		{"low to high", "low", "high"},
		{"xhigh to max", "xhigh", "max"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","reasoning_effort":"` + tt.inputLevel + `","messages":[{"role":"user","content":"hi"}]}`)
			got := sanitize.TransformRequestBody(body, cfg)

			if string(got) == string(body) {
				t.Fatal("TransformRequestBody should have remapped reasoning_effort, but returned same body")
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(got, &raw); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}

			model, _ := util.RawStringField(raw, "model")
			if model != "deepseek-v4-pro" {
				t.Errorf("model = %q, want deepseek-v4-pro", model)
			}

			var effort string
			if err := json.Unmarshal(raw["reasoning_effort"], &effort); err != nil {
				t.Fatalf("unmarshal reasoning_effort: %v", err)
			}
			if effort != tt.wantLevel {
				t.Errorf("reasoning_effort = %q, want %q", effort, tt.wantLevel)
			}
		})
	}
}

func TestTransformRequestBody_NonDeepSeekNonGLMNoStrip(t *testing.T) {
	cfg := &config.Config{
		ModelRemap: map[string]string{"mimo-v2.5": "standard"},
		ModelTiers: map[string]string{"standard": "mimo-v2.5"},
		// No strip_fields for this tier — should not strip anything
	}

	body := []byte(`{"model":"mimo-v2.5","prompt_cache_retention":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	// mimo-v2.5 is neither deepseek nor glm, has no matching strip_fields;
	// model remap mimo-v2.5 → standard → mimo-v2.5 (no-op). Body unchanged.
	assertBodyUnchanged(t, got, body)
}
