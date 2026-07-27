package sanitize_test

import (
	"encoding/json"
	"os"
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

// ── New generic effort mapping tests ─────────────────────────────────

func TestTransformRequestBody_GLM52_EffortMap(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"glm-5.2": "glm-tier"},
		ModelTiers:        map[string]string{"glm-tier": "glm-5.2"},
	}

	body := []byte(`{"model":"glm-5.2","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	if string(got) == string(body) {
		t.Fatal("TransformRequestBody should have applied effort mapping, body unchanged")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be xhigh (1:1 mapping)
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "xhigh" {
		t.Errorf("reasoning_effort = %q, want xhigh", got)
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_GLM51_Toggle(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"glm-5.1": "glm51-tier"},
		ModelTiers:        map[string]string{"glm51-tier": "glm-5.1"},
	}

	body := []byte(`{"model":"glm-5.1","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	if string(got) == string(body) {
		t.Fatal("body should have changed")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_KimiK3_EffortClampDown(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"kimi-k3": "k3-tier"},
		ModelTiers:        map[string]string{"k3-tier": "kimi-k3"},
	}

	body := []byte(`{"model":"kimi-k3","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// medium → low (clamp down)
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "low" {
		t.Errorf("reasoning_effort = %q, want low", got)
	}
}

func TestTransformRequestBody_KimiK26_Toggle(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"kimi-k2.6": "k26-tier"},
		ModelTiers:        map[string]string{"k26-tier": "kimi-k2.6"},
	}

	body := []byte(`{"model":"kimi-k2.6","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_Qwen37_Budget(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"qwen3.7-plus": "qwen37-tier"},
		ModelTiers:        map[string]string{"qwen37-tier": "qwen3.7-plus"},
	}

	body := []byte(`{"model":"qwen3.7-plus","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// enable_thinking should be true
	if !util.RawBoolField(raw, "enable_thinking") {
		t.Error("enable_thinking should be true")
	}

	// thinking_budget should be 81920
	var budget int
	if err := json.Unmarshal(raw["thinking_budget"], &budget); err != nil {
		t.Fatalf("thinking_budget not an int: %v", err)
	}
	if budget != 81920 {
		t.Errorf("thinking_budget = %d, want 81920", budget)
	}
}

func TestTransformRequestBody_Qwen36_Budget(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"qwen3.6-plus": "qwen36-tier"},
		ModelTiers:        map[string]string{"qwen36-tier": "qwen3.6-plus"},
	}

	body := []byte(`{"model":"qwen3.6-plus","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// thinking_budget should be 81920 (not 262144)
	var budget int
	if err := json.Unmarshal(raw["thinking_budget"], &budget); err != nil {
		t.Fatalf("thinking_budget not an int: %v", err)
	}
	if budget != 81920 {
		t.Errorf("thinking_budget = %d, want 81920", budget)
	}

	// enable_thinking should be true
	if !util.RawBoolField(raw, "enable_thinking") {
		t.Error("enable_thinking should be true")
	}
}

func TestTransformRequestBody_Hy3_EffortClampDown(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"hy3": "hy3-tier"},
		ModelTiers:        map[string]string{"hy3-tier": "hy3"},
	}

	body := []byte(`{"model":"hy3","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// medium → low (downward proximity)
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "low" {
		t.Errorf("reasoning_effort = %q, want low", got)
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_Hy3_XhighClamp(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"hy3": "hy3-tier"},
		ModelTiers:        map[string]string{"hy3-tier": "hy3"},
	}

	body := []byte(`{"model":"hy3","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// xhigh → high (clamp to max)
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "high" {
		t.Errorf("reasoning_effort = %q, want high", got)
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_MiMo_Toggle(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"mimo-v2.5": "mimo-tier"},
		ModelTiers:        map[string]string{"mimo-tier": "mimo-v2.5"},
	}

	body := []byte(`{"model":"mimo-v2.5","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
}

func TestTransformRequestBody_MiniMaxM3_Off(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"minimax-m3": "m3-tier"},
		ModelTiers:        map[string]string{"m3-tier": "minimax-m3"},
	}

	body := []byte(`{"model":"minimax-m3","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// thinking should be "disabled"
	var thinking string
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("thinking is not a string: %v", err)
	}
	if thinking != "disabled" {
		t.Errorf("thinking = %q, want disabled", thinking)
	}
}

func TestTransformRequestBody_MiniMaxM2_ForceOn(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"minimax-m2.7": "m27-tier"},
		ModelTiers:        map[string]string{"m27-tier": "minimax-m2.7"},
	}

	body := []byte(`{"model":"minimax-m2.7","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// thinking should be "adaptive" (forced on)
	var thinking string
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("thinking is not a string: %v", err)
	}
	if thinking != "adaptive" {
		t.Errorf("thinking = %q, want adaptive", thinking)
	}
}

func TestTransformRequestBody_NoThinkingModel_Strip(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"unknown-v1": "unknown-tier"},
		ModelTiers:        map[string]string{"unknown-tier": "grok-4.5"},
	}

	body := []byte(`{"model":"unknown-v1","reasoning_effort":"high","thinking":{"level":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Both should be stripped for unsupported model
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped for unsupported model")
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("thinking should be stripped for unsupported model")
	}
}

func TestTransformRequestBody_ThinkingLevelPath(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"glm-5.2": "glm-tier"},
		ModelTiers:        map[string]string{"glm-tier": "glm-5.2"},
	}

	// Use thinking.level (no reasoning_effort)
	body := []byte(`{"model":"glm-5.2","thinking":{"level":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be written from thinking.level mapping
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "xhigh" {
		t.Errorf("reasoning_effort = %q, want xhigh", got)
	}

	// thinking.type should be enabled
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", typ)
	}
	// thinking.level should have been cleaned up (not in EnumFields for glm-5.2)
	if _, exists := thinking["level"]; exists {
		t.Error("thinking.level should have been removed")
	}
}

func TestTransformRequestBody_NoEffortField_NoOp(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"glm-5.2": "glm-tier"},
		ModelTiers:        map[string]string{"glm-tier": "glm-5.2"},
	}

	// No reasoning_effort or thinking.level fields
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	got := sanitize.TransformRequestBody(body, cfg)

	// Model remap: glm-5.2 → glm-tier → glm-5.2 (identity). Body unchanged.
	assertBodyUnchanged(t, got, body)
}

func TestTransformRequestBody_Off_DeepSeekPassthrough(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"gpt-5.5": "frontier"},
		ModelTiers:        map[string]string{"frontier": "deepseek-v4-pro"},
	}

	body := []byte(`{"model":"gpt-5.5","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Model should be remapped, but reasoning_effort=off should remain untouched
	model, _ := util.RawStringField(raw, "model")
	if model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want deepseek-v4-pro", model)
	}

	// reasoning_effort should still be "off" (passthrough)
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "off" {
		t.Errorf("reasoning_effort = %q, want off (passthrough)", got)
	}
}

func TestTransformRequestBody_Qwen_Off_StripsBudget(t *testing.T) {
	cfg := &config.Config{
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"qwen3.7-plus": "qwen37-tier"},
		ModelTiers:        map[string]string{"qwen37-tier": "qwen3.7-plus"},
	}

	body := []byte(`{"model":"qwen3.7-plus","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBody(body, cfg)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}

	// enable_thinking should be false
	if util.RawBoolField(raw, "enable_thinking") {
		t.Error("enable_thinking should be false for off")
	}

	// thinking_budget should be deleted
	if _, ok := raw["thinking_budget"]; ok {
		t.Error("thinking_budget should be deleted for off")
	}
}

// ── Provider-dimension effort schema tests ──────────────────────────────

// testProvidersYAML configures two providers: an ollama-cloud host (→ ollama
// schema) and an opencode-go host (→ opencode schema). EffortSchema is derived
// from the account base_url hosts, so no extra YAML fields are needed.
const testProvidersYAML = `
providers:
  ollama-cloud:
    accounts:
      - name: ollama-acc
        key: test-key-12345
        base_url: https://ollama.com/v1
  opencode-go:
    accounts:
      - name: opencode-acc
        key: test-key-12345
        base_url: https://opencode.ai/zen
`

func loadProviderCfg(t testing.TB) *config.Config {
	t.Helper()
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if _, err := f.Write([]byte(testProvidersYAML)); err != nil {
		f.Close()
		os.Remove(name)
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(name)

	cfg, err := config.LoadConfig(name)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestTransformRequestBody_Ollama_GLM52(t *testing.T) {
	cfg := loadProviderCfg(t)
	body := []byte(`{"model":"glm-5.2","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBodyForProvider(body, cfg, "ollama-cloud")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "max" {
		t.Errorf("reasoning_effort=%q, want max", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama glm-5.2 should not set thinking.type")
	}
}

func TestTransformRequestBody_Opencode_GLM52(t *testing.T) {
	cfg := loadProviderCfg(t)
	body := []byte(`{"model":"glm-5.2","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBodyForProvider(body, cfg, "opencode-go")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "xhigh" {
		t.Errorf("reasoning_effort=%q, want xhigh (opencode 1:1)", got)
	}
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("thinking not object: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestTransformRequestBody_CrossProvider_DeepSeek(t *testing.T) {
	cfg := loadProviderCfg(t)

	t.Run("opencode deepseek xhigh double-write", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"xhigh","thinking":{"level":"xhigh"}}`)
		got := sanitize.TransformRequestBodyForProvider(body, cfg, "opencode-go")
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(got, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := unmarshalString(t, raw["reasoning_effort"]); got != "max" {
			t.Errorf("reasoning_effort=%q, want max", got)
		}
		var thinking map[string]any
		if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
			t.Fatalf("thinking not object: %v", err)
		}
		if lvl, _ := thinking["level"].(string); lvl != "max" {
			t.Errorf("thinking.level=%q, want max (DeepSeekCompat double-write)", lvl)
		}
	})

	t.Run("ollama deepseek xhigh reasoning_effort only", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"xhigh","thinking":{"level":"xhigh"}}`)
		got := sanitize.TransformRequestBodyForProvider(body, cfg, "ollama-cloud")
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(got, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := unmarshalString(t, raw["reasoning_effort"]); got != "max" {
			t.Errorf("reasoning_effort=%q, want max", got)
		}
		if _, ok := raw["thinking"]; ok {
			t.Error("ollama deepseek should drop thinking object (DEFAULT profile, no double-write)")
		}
	})

	t.Run("opencode deepseek off passthrough", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"off"}`)
		got := sanitize.TransformRequestBodyForProvider(body, cfg, "opencode-go")
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(got, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := unmarshalString(t, raw["reasoning_effort"]); got != "off" {
			t.Errorf("reasoning_effort=%q, want off (passthrough)", got)
		}
	})

	t.Run("ollama deepseek off sets none", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"off"}`)
		got := sanitize.TransformRequestBodyForProvider(body, cfg, "ollama-cloud")
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(got, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := unmarshalString(t, raw["reasoning_effort"]); got != "none" {
			t.Errorf("reasoning_effort=%q, want none (ollama DEFAULT, set value)", got)
		}
	})
}

func TestTransformRequestBody_Ollama_OffSetsNone(t *testing.T) {
	cfg := loadProviderCfg(t)
	body := []byte(`{"model":"glm-5.2","reasoning_effort":"off","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBodyForProvider(body, cfg, "ollama-cloud")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// reasoning_effort still present, value=none (not deleted)
	if _, ok := raw["reasoning_effort"]; !ok {
		t.Fatal("reasoning_effort should still be present (set to none, not deleted)")
	}
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "none" {
		t.Errorf("reasoning_effort=%q, want none", got)
	}
}

func TestTransformRequestBody_EmptyProvider_DefaultOpencode(t *testing.T) {
	cfg := loadProviderCfg(t)
	body := []byte(`{"model":"glm-5.2","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	got := sanitize.TransformRequestBodyForProvider(body, cfg, "") // empty provider → opencode

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := unmarshalString(t, raw["reasoning_effort"]); got != "xhigh" {
		t.Errorf("reasoning_effort=%q, want xhigh (opencode 1:1)", got)
	}
	var thinking map[string]any
	if err := json.Unmarshal(raw["thinking"], &thinking); err != nil {
		t.Fatalf("thinking not object: %v", err)
	}
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

// unmarshalString is a test helper that extracts a string from json.RawMessage.
func unmarshalString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal string: %v (raw=%s)", err, string(raw))
	}
	return s
}
