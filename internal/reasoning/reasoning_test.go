package reasoning

import (
	"encoding/json"
	"testing"

	"github.com/dorokuma/prism/internal/util"
)

// ── ProfileFor tests ─────────────────────────────────────────────────

func TestProfileFor_Order(t *testing.T) {
	t.Run("glm-5.2 not swallowed by glm-5", func(t *testing.T) {
		p := ProfileFor("glm-5.2-v2")
		if p.Form != FormEnum {
			t.Fatalf("glm-5.2: got Form=%s, want enum", p.Form)
		}
		// ToggleField should be set for glm-5.2
		if p.ToggleField != "thinking.type" {
			t.Errorf("glm-5.2 ToggleField=%q, want thinking.type", p.ToggleField)
		}
	})

	t.Run("glm-5 falls to toggle", func(t *testing.T) {
		p := ProfileFor("glm-5")
		if p.Form != FormToggle {
			t.Fatalf("glm-5: got Form=%s, want toggle", p.Form)
		}
	})

	t.Run("minimax-m2.7 not mis-matched", func(t *testing.T) {
		p := ProfileFor("minimax-m2.7")
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
		p := ProfileFor("minimax-m3")
		if p.Form != FormToggle {
			t.Fatalf("minimax-m3: got Form=%s, want toggle", p.Form)
		}
		if p.ForceOn {
			t.Error("minimax-m3 should have ForceOn=false")
		}
	})

	t.Run("mimo- not confused with minimax", func(t *testing.T) {
		p := ProfileFor("mimo-v2.5")
		if p.Form != FormToggle {
			t.Fatalf("mimo-v2.5: got Form=%s, want toggle", p.Form)
		}
		if p.ToggleField != "thinking.type" {
			t.Errorf("mimo-v2.5 ToggleField=%q, want thinking.type", p.ToggleField)
		}
	})

	t.Run("deepseek prefix", func(t *testing.T) {
		p := ProfileFor("deepseek-v4-pro")
		if p.Form != FormEnum {
			t.Fatalf("deepseek: got Form=%s, want enum", p.Form)
		}
		if !p.DeepSeekCompat {
			t.Error("deepseek should have DeepSeekCompat=true")
		}
	})

	t.Run("qwen3.7-plus is budget", func(t *testing.T) {
		p := ProfileFor("qwen3.7-plus-v1")
		if p.Form != FormBudget {
			t.Fatalf("qwen3.7-plus: got Form=%s, want budget", p.Form)
		}
		if p.BudgetMap["xhigh"] != 81920 {
			t.Errorf("qwen3.7-plus BudgetMap[xhigh]=%d, want 81920", p.BudgetMap["xhigh"])
		}
	})

	t.Run("qwen3.6-plus budget 81920 not 262144", func(t *testing.T) {
		p := ProfileFor("qwen3.6-plus")
		if p.BudgetMax != 81920 {
			t.Errorf("qwen3.6-plus BudgetMax=%d, want 81920", p.BudgetMax)
		}
	})
}

func TestProfileFor_Unknown(t *testing.T) {
	p := ProfileFor("grok-4.5")
	if p.Form != FormNone {
		t.Errorf("grok-4.5: got Form=%s, want none", p.Form)
	}

	p = ProfileFor("")
	if p.Form != FormNone {
		t.Errorf("empty: got Form=%s, want none", p.Form)
	}

	p = ProfileFor("non-existent-model-123")
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
