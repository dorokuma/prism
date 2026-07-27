package reasoning

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dorokuma/prism/internal/util"
)

// ── ProfileFor tests ─────────────────────────────────────────────────

func TestProfileFor_Order(t *testing.T) {
	t.Run("glm-5.2 not swallowed by glm-5", func(t *testing.T) {
		p := ProfileForModel("glm-5.2-v2")
		if p.Form != FormEnum {
			t.Fatalf("glm-5.2: got Form=%s, want enum", p.Form)
		}
		// ToggleField should be set for glm-5.2
		if p.ToggleField != "thinking.type" {
			t.Errorf("glm-5.2 ToggleField=%q, want thinking.type", p.ToggleField)
		}
	})

	t.Run("glm-5 falls to toggle", func(t *testing.T) {
		p := ProfileForModel("glm-5")
		if p.Form != FormToggle {
			t.Fatalf("glm-5: got Form=%s, want toggle", p.Form)
		}
	})

	t.Run("minimax-m2.7 not mis-matched", func(t *testing.T) {
		p := ProfileForModel("minimax-m2.7")
		if p.Form != FormToggle {
			t.Fatalf("minimax-m2.7: got Form=%s, want toggle", p.Form)
		}
		if !p.ForceOn {
			t.Error("minimax-m2.7 should have ForceOn=true")
		}
		if p.ToggleField != "thinking" {
			t.Errorf("minimax-m2.7 ToggleField=%q, want thinking", p.ToggleField)
		}
	})

	t.Run("minimax-m3 is toggle switchable", func(t *testing.T) {
		p := ProfileForModel("minimax-m3")
		if p.Form != FormToggle {
			t.Fatalf("minimax-m3: got Form=%s, want toggle", p.Form)
		}
		if p.ForceOn {
			t.Error("minimax-m3 should have ForceOn=false")
		}
	})

	t.Run("mimo- not confused with minimax", func(t *testing.T) {
		p := ProfileForModel("mimo-v2.5")
		if p.Form != FormToggle {
			t.Fatalf("mimo-v2.5: got Form=%s, want toggle", p.Form)
		}
		if p.ToggleField != "thinking.type" {
			t.Errorf("mimo-v2.5 ToggleField=%q, want thinking.type", p.ToggleField)
		}
	})

	t.Run("deepseek prefix", func(t *testing.T) {
		p := ProfileForModel("deepseek-v4-pro")
		if p.Form != FormEnum {
			t.Fatalf("deepseek: got Form=%s, want enum", p.Form)
		}
		if !p.DeepSeekCompat {
			t.Error("deepseek should have DeepSeekCompat=true")
		}
	})

	t.Run("qwen3.7-plus is budget", func(t *testing.T) {
		p := ProfileForModel("qwen3.7-plus-v1")
		if p.Form != FormBudget {
			t.Fatalf("qwen3.7-plus: got Form=%s, want budget", p.Form)
		}
		if p.BudgetMap["xhigh"] != 81920 {
			t.Errorf("qwen3.7-plus BudgetMap[xhigh]=%d, want 81920", p.BudgetMap["xhigh"])
		}
	})

	t.Run("qwen3.6-plus budget 81920 not 262144", func(t *testing.T) {
		p := ProfileForModel("qwen3.6-plus")
		if p.BudgetMax != 81920 {
			t.Errorf("qwen3.6-plus BudgetMax=%d, want 81920", p.BudgetMax)
		}
	})
}

func TestProfileFor_Unknown(t *testing.T) {
	p := ProfileForModel("grok-4.5")
	if p.Form != FormNone {
		t.Errorf("grok-4.5: got Form=%s, want none", p.Form)
	}

	p = ProfileForModel("")
	if p.Form != FormNone {
		t.Errorf("empty: got Form=%s, want none", p.Form)
	}

	p = ProfileForModel("non-existent-model-123")
	if p.Form != FormNone {
		t.Errorf("unknown: got Form=%s, want none", p.Form)
	}
}

// ── Apply helpers ────────────────────────────────────────────────────

func rawFromJSON(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func mustJSON(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func strField(t *testing.T, raw map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw[key], &s); err != nil {
		t.Fatalf("field %q not a string (%v): raw=%s", key, err, string(raw[key]))
	}
	return s
}

// ── Apply: DeepSeek compat ──────────────────────────────────────────

func TestApply_DeepSeekThinkingLevel(t *testing.T) {
	// thinking.level=low → high
	raw := rawFromJSON(t, `{"thinking":{"level":"low"}}`)
	changed := Apply(raw, "deepseek-v4-pro")
	if !changed {
		t.Fatal("expected change")
	}
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if level, _ := thinking["level"].(string); level != "high" {
		t.Errorf("thinking.level=%q, want high", level)
	}
}

func TestApply_DeepSeekReasoningEffort(t *testing.T) {
	// reasoning_effort=xhigh → max
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	changed := Apply(raw, "deepseek-v4-pro")
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "max" {
		t.Errorf("reasoning_effort=%q, want max", got)
	}
}

func TestApply_DeepSeekOffPassthrough(t *testing.T) {
	// off should be left untouched (passthrough)
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := Apply(raw, "deepseek-v4-pro")
	if changed {
		t.Error("DeepSeek off should be passthrough, no change")
	}
	// off should still be there
	if got := strField(t, raw, "reasoning_effort"); got != "off" {
		t.Errorf("reasoning_effort=%q, want off", got)
	}
}

func TestApply_DeepSeekNoFields(t *testing.T) {
	raw := rawFromJSON(t, `{"model":"deepseek"}`)
	changed := Apply(raw, "deepseek-v4-pro")
	if changed {
		t.Error("no thinking fields → no change")
	}
}

// ── Apply: FormEnum ─────────────────────────────────────────────────

func TestApply_GLM52_EffortMap(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	changed := Apply(raw, "glm-5.2")
	if !changed {
		t.Fatal("expected change")
	}
	// xhigh in → xhigh out (1:1)
	if got := strField(t, raw, "reasoning_effort"); got != "xhigh" {
		t.Errorf("reasoning_effort=%q, want xhigh", got)
	}
	// thinking.type should be enabled
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestApply_GLM52_ThinkingLevelPath(t *testing.T) {
	// thinking.level=high (no reasoning_effort) → write reasoning_effort=high + thinking.type=enabled
	raw := rawFromJSON(t, `{"thinking":{"level":"high"}}`)
	changed := Apply(raw, "glm-5.2")
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "high" {
		t.Errorf("reasoning_effort=%q, want high", got)
	}
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
	// thinking.level should have been cleaned up (not in EnumFields)
	if _, exists := thinking["level"]; exists {
		t.Error("thinking.level should have been removed")
	}
}

func TestApply_GLM52_NoEffortField(t *testing.T) {
	raw := rawFromJSON(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	changed := Apply(raw, "glm-5.2")
	if changed {
		t.Error("no thinking fields → no change")
	}
}

func TestApply_GLM52_Off(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := Apply(raw, "glm-5.2")
	if !changed {
		t.Fatal("expected change")
	}
	// reasoning_effort should be deleted
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be deleted for off")
	}
	// thinking.type should be disabled
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "disabled" {
		t.Errorf("thinking.type=%q, want disabled", typ)
	}
}

func TestApply_KimiK3_EffortClampDown(t *testing.T) {
	// medium → low (clamp down)
	raw := rawFromJSON(t, `{"reasoning_effort":"medium"}`)
	changed := Apply(raw, "kimi-k3")
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "low" {
		t.Errorf("reasoning_effort=%q, want low", got)
	}
}

func TestApply_Hy3_EffortClampDown(t *testing.T) {
	// medium → low + thinking.type=enabled
	raw := rawFromJSON(t, `{"reasoning_effort":"medium"}`)
	changed := Apply(raw, "hy3")
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "low" {
		t.Errorf("reasoning_effort=%q, want low", got)
	}
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestApply_Hy3_XhighClamp(t *testing.T) {
	// xhigh → high (clamp to max)
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	changed := Apply(raw, "hy3")
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "high" {
		t.Errorf("reasoning_effort=%q, want high", got)
	}
}

// ── Apply: FormToggle ────────────────────────────────────────────────

func TestApply_GLM51_Toggle(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"high"}`)
	changed := Apply(raw, "glm-5.1")
	if !changed {
		t.Fatal("expected change")
	}
	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}
	// thinking.type should be enabled
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestApply_KimiK26_Toggle(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"high"}`)
	changed := Apply(raw, "kimi-k2.6")
	if !changed {
		t.Fatal("expected change")
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestApply_MiMo_Toggle(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"high"}`)
	changed := Apply(raw, "mimo-v2.5")
	if !changed {
		t.Fatal("expected change")
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}
	var thinking map[string]any
	_ = json.Unmarshal(raw["thinking"], &thinking)
	if typ, _ := thinking["type"].(string); typ != "enabled" {
		t.Errorf("thinking.type=%q, want enabled", typ)
	}
}

func TestApply_MiniMaxM3_Off(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := Apply(raw, "minimax-m3")
	if !changed {
		t.Fatal("expected change")
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
		t.Errorf("thinking=%q, want disabled", thinking)
	}
}

func TestApply_MiniMaxM2_ForceOn(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := Apply(raw, "minimax-m2.7")
	if !changed {
		t.Fatal("expected change")
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
		t.Errorf("thinking=%q, want adaptive", thinking)
	}
}

// ── Apply: FormBudget ────────────────────────────────────────────────

func TestApply_Qwen37_Budget(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	changed := Apply(raw, "qwen3.7-plus")
	if !changed {
		t.Fatal("expected change")
	}
	// reasoning_effort should be stripped
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped")
	}
	// enable_thinking
	if !util.RawBoolField(raw, "enable_thinking") {
		t.Error("enable_thinking should be true")
	}
	// thinking_budget should be 81920
	var budget int
	if err := json.Unmarshal(raw["thinking_budget"], &budget); err != nil {
		t.Fatalf("thinking_budget not an int: %v", err)
	}
	if budget != 81920 {
		t.Errorf("thinking_budget=%d, want 81920", budget)
	}
}

func TestApply_Qwen_Off_StripsBudget(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := Apply(raw, "qwen3.7-plus")
	if !changed {
		t.Fatal("expected change")
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

// ── Apply: FormNone ─────────────────────────────────────────────────

func TestApply_NoThinkingModel_Strip(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"high","thinking":{"level":"high"}}`)
	changed := Apply(raw, "grok-4.5")
	if !changed {
		t.Fatal("expected change")
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be stripped for unknown model")
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("thinking should be stripped for unknown model")
	}
}

func TestApply_FormNone_NoFields(t *testing.T) {
	raw := rawFromJSON(t, `{"model":"unknown"}`)
	changed := Apply(raw, "grok-4.5")
	if changed {
		t.Error("no thinking fields → no change")
	}
}

// ── ProfileFor: ollama schema ───────────────────────────────────────

func TestProfileFor_Ollama_GLM52(t *testing.T) {
	p := ProfileFor("glm-5.2", SchemaOllama)
	if p.Form != FormEnum {
		t.Fatalf("glm-5.2 ollama: got Form=%s, want enum", p.Form)
	}
	if p.EffortMap["xhigh"] != "max" {
		t.Errorf("glm-5.2 ollama EffortMap[xhigh]=%q, want max", p.EffortMap["xhigh"])
	}
	if p.ToggleField != "" {
		t.Errorf("glm-5.2 ollama ToggleField=%q, want empty", p.ToggleField)
	}
	if p.OffValue != "none" {
		t.Errorf("glm-5.2 ollama OffValue=%q, want none", p.OffValue)
	}
}

func TestProfileFor_Ollama_Qwen3VL_Before_Qwen3(t *testing.T) {
	pVL := ProfileFor("qwen3-vl", SchemaOllama)
	if !pVL.ForceOn {
		t.Error("qwen3-vl should have ForceOn=true (NO_OFF)")
	}
	if pVL.OffValue != "low" {
		t.Errorf("qwen3-vl OffValue=%q, want low", pVL.OffValue)
	}
	if pVL.EffortMap["xhigh"] != "max" {
		t.Errorf("qwen3-vl EffortMap[xhigh]=%q, want max", pVL.EffortMap["xhigh"])
	}

	pQ := ProfileFor("qwen3", SchemaOllama)
	if pQ.ForceOn {
		t.Error("qwen3 should have ForceOn=false (binary)")
	}
	if pQ.EffortMap["low"] != "medium" {
		t.Errorf("qwen3 EffortMap[low]=%q, want medium", pQ.EffortMap["low"])
	}
	if pQ.EffortMap["xhigh"] != "medium" {
		t.Errorf("qwen3 EffortMap[xhigh]=%q, want medium (binary clamps to medium)", pQ.EffortMap["xhigh"])
	}
	if pQ.OffValue != "none" {
		t.Errorf("qwen3 OffValue=%q, want none", pQ.OffValue)
	}
}

func TestProfileFor_Ollama_Default_Fallback(t *testing.T) {
	p := ProfileFor("deepseek-v4-pro", SchemaOllama)
	if p.Form != FormEnum {
		t.Fatalf("ollama default: got Form=%s, want enum (not FormNone)", p.Form)
	}
	if p.DeepSeekCompat {
		t.Error("ollama deepseek-v4-pro should NOT use DeepSeekCompat (uses default ollama profile)")
	}
	if p.OffValue != "none" {
		t.Errorf("ollama default OffValue=%q, want none", p.OffValue)
	}
}

func TestProfileFor_NonCrosstalk_SameModel(t *testing.T) {
	opencodeGLM := ProfileFor("glm-5.2", SchemaOpencode)
	ollamaGLM := ProfileFor("glm-5.2", SchemaOllama)
	if opencodeGLM.ToggleField == ollamaGLM.ToggleField {
		t.Error("glm-5.2 should differ across schemas (toggle field)")
	}
	if opencodeGLM.ToggleField != "thinking.type" {
		t.Errorf("opencode glm-5.2 ToggleField=%q, want thinking.type", opencodeGLM.ToggleField)
	}
	if ollamaGLM.ToggleField != "" {
		t.Errorf("ollama glm-5.2 ToggleField=%q, want empty", ollamaGLM.ToggleField)
	}

	opencodeDS := ProfileFor("deepseek-v4-pro", SchemaOpencode)
	ollamaDS := ProfileFor("deepseek-v4-pro", SchemaOllama)
	if !opencodeDS.DeepSeekCompat {
		t.Error("opencode deepseek-v4-pro should have DeepSeekCompat=true")
	}
	if ollamaDS.DeepSeekCompat {
		t.Error("ollama deepseek-v4-pro should have DeepSeekCompat=false")
	}
}

// ── Apply: ollama schema ────────────────────────────────────────────

func TestApply_Ollama_GLM52_Xhigh(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	changed := ApplyWithSchema(raw, "glm-5.2", SchemaOllama)
	if !changed {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "max" {
		t.Errorf("reasoning_effort=%q, want max", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama glm-5.2 should not set thinking.type")
	}
}

func TestApply_Ollama_GLM52_Off(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := ApplyWithSchema(raw, "glm-5.2", SchemaOllama)
	if !changed {
		t.Fatal("expected change")
	}
	// off → reasoning_effort=none (set value, not delete)
	if got := strField(t, raw, "reasoning_effort"); got != "none" {
		t.Errorf("reasoning_effort=%q, want none (set value, not delete)", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama glm-5.2 should not set thinking.type")
	}
}

func TestApply_Ollama_GLM52_LowClampsUp(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"low"}`)
	changed := ApplyWithSchema(raw, "glm-5.2", SchemaOllama)
	if !changed {
		t.Fatal("expected change")
	}
	// glm-5.2 only exposes off/high/max; low/med clamp up to high (keeps thinking on)
	if got := strField(t, raw, "reasoning_effort"); got != "high" {
		t.Errorf("reasoning_effort=%q, want high (clamp up, keep thinking)", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama glm-5.2 should not set thinking.type")
	}
}

func TestApply_Ollama_GPTOSS_OffForceOn(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := ApplyWithSchema(raw, "gpt-oss", SchemaOllama)
	if !changed {
		t.Fatal("expected change")
	}
	// off → low (set value, not delete) + Warn
	if got := strField(t, raw, "reasoning_effort"); got != "low" {
		t.Errorf("gpt-oss off: reasoning_effort=%q, want low", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama gpt-oss should not set thinking.type")
	}

	raw2 := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	if !ApplyWithSchema(raw2, "gpt-oss", SchemaOllama) {
		t.Fatal("expected change")
	}
	// xhigh clamped to high (no max exposed)
	if got := strField(t, raw2, "reasoning_effort"); got != "high" {
		t.Errorf("gpt-oss xhigh: reasoning_effort=%q, want high (clamp)", got)
	}
}

func TestApply_Ollama_Qwen3_Binary(t *testing.T) {
	cases := map[string]string{
		"off":    "none",
		"low":    "medium",
		"medium": "medium",
		"high":   "medium",
		"xhigh":  "medium",
	}
	for in, want := range cases {
		raw := rawFromJSON(t, fmt.Sprintf(`{"reasoning_effort":%q}`, in))
		if !ApplyWithSchema(raw, "qwen3", SchemaOllama) {
			t.Fatalf("qwen3 %s: expected change", in)
		}
		if got := strField(t, raw, "reasoning_effort"); got != want {
			t.Errorf("qwen3 %s: reasoning_effort=%q, want %q", in, got, want)
		}
		if _, ok := raw["thinking"]; ok {
			t.Errorf("qwen3 %s: ollama should not set thinking.type", in)
		}
	}
}

func TestApply_Ollama_KimiK2Thinking_OffForceOn(t *testing.T) {
	raw := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	changed := ApplyWithSchema(raw, "kimi-k2-thinking", SchemaOllama)
	if !changed {
		t.Fatal("expected change")
	}
	// off → low (set value) + Warn
	if got := strField(t, raw, "reasoning_effort"); got != "low" {
		t.Errorf("kimi-k2-thinking off: reasoning_effort=%q, want low", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama kimi-k2-thinking should not set thinking.type")
	}
}

func TestApply_Ollama_Default_DeepSeek(t *testing.T) {
	// deepseek-v4-pro under ollama schema uses the DEFAULT profile:
	// xhigh → max (reasoning_effort only), off → none; no thinking.type.
	raw := rawFromJSON(t, `{"reasoning_effort":"xhigh"}`)
	if !ApplyWithSchema(raw, "deepseek-v4-pro", SchemaOllama) {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "max" {
		t.Errorf("ollama deepseek xhigh: reasoning_effort=%q, want max", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama deepseek should not set thinking.type")
	}

	raw2 := rawFromJSON(t, `{"reasoning_effort":"off"}`)
	if !ApplyWithSchema(raw2, "deepseek-v4-pro", SchemaOllama) {
		t.Fatal("expected change")
	}
	if got := strField(t, raw2, "reasoning_effort"); got != "none" {
		t.Errorf("ollama deepseek off: reasoning_effort=%q, want none", got)
	}
}

func TestApply_Ollama_ThinkingLevelPath(t *testing.T) {
	raw := rawFromJSON(t, `{"thinking":{"level":"high"}}`)
	if !ApplyWithSchema(raw, "glm-5.2", SchemaOllama) {
		t.Fatal("expected change")
	}
	if got := strField(t, raw, "reasoning_effort"); got != "high" {
		t.Errorf("reasoning_effort=%q, want high", got)
	}
	if _, ok := raw["thinking"]; ok {
		t.Error("ollama should not keep thinking object (no thinking.type)")
	}
}
