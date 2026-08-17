package usagemeta

import "testing"

func TestParseOpenAI(t *testing.T) {
	t.Run("full_fields", func(t *testing.T) {
		body := []byte(`{
			"id":"chatcmpl-1","object":"chat.completion",
			"usage":{
				"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,
				"prompt_cache_hit_tokens":200,"prompt_cache_miss_tokens":800,
				"prompt_tokens_details":{"cached_tokens":200},
				"completion_tokens_details":{"reasoning_tokens":100}
			}
		}`)
		u := ParseOpenAI(body)
		if u.Prompt != 1000 {
			t.Errorf("Prompt = %d, want 1000", u.Prompt)
		}
		if u.Completion != 500 {
			t.Errorf("Completion = %d, want 500", u.Completion)
		}
		if u.Total != 1500 {
			t.Errorf("Total = %d, want 1500", u.Total)
		}
		if u.Cached != 200 {
			t.Errorf("Cached = %d, want 200", u.Cached)
		}
		if u.Reasoning != 100 {
			t.Errorf("Reasoning = %d, want 100", u.Reasoning)
		}
		if u.CacheWrite != 0 {
			t.Errorf("CacheWrite = %d, want 0", u.CacheWrite)
		}
		if u.Source != SourceOpenAI {
			t.Errorf("Source = %q, want %q", u.Source, SourceOpenAI)
		}
	})

	t.Run("cached_falls_back_to_prompt_tokens_details", func(t *testing.T) {
		// prompt_cache_hit_tokens absent → cached comes from
		// prompt_tokens_details.cached_tokens (mirrors the pre-existing
		// stream translation logic).
		body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":7}}}`)
		u := ParseOpenAI(body)
		if u.Cached != 7 {
			t.Errorf("Cached = %d, want 7 (from prompt_tokens_details)", u.Cached)
		}
	})

	t.Run("no_math_on_reasoning_and_cached", func(t *testing.T) {
		// reasoning_tokens (100) is already included in completion_tokens
		// (500); cached_tokens (200) is already included in prompt_tokens
		// (1000). The parser must record both as-is — no addition, no
		// subtraction.
		body := []byte(`{"usage":{
			"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,
			"prompt_cache_hit_tokens":200,
			"completion_tokens_details":{"reasoning_tokens":100}
		}}`)
		u := ParseOpenAI(body)
		if u.Completion != 500 {
			t.Errorf("Completion = %d, want 500 (reasoning must not be subtracted)", u.Completion)
		}
		if u.Reasoning != 100 {
			t.Errorf("Reasoning = %d, want 100", u.Reasoning)
		}
		if u.Prompt != 1000 {
			t.Errorf("Prompt = %d, want 1000 (cached must not be subtracted)", u.Prompt)
		}
		if u.Cached != 200 {
			t.Errorf("Cached = %d, want 200", u.Cached)
		}
	})

	t.Run("empty_and_garbage", func(t *testing.T) {
		for _, body := range [][]byte{nil, {}, []byte("not json"), []byte(`{"usage":null}`), []byte(`{}`)} {
			u := ParseOpenAI(body)
			if u != (Usage{}) {
				t.Errorf("ParseOpenAI(%q) = %+v, want zero Usage", body, u)
			}
		}
	})

	t.Run("absent_total_tokens_falls_back", func(t *testing.T) {
		u := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
		if u.Prompt != 100 || u.Completion != 50 {
			t.Fatalf("Prompt/Completion = %d/%d, want 100/50", u.Prompt, u.Completion)
		}
		if u.Total != 150 {
			t.Errorf("Total = %d, want 150 (missing total_tokens → prompt+completion)", u.Total)
		}
		if u.Source != SourceOpenAI {
			t.Errorf("Source = %q, want %q", u.Source, SourceOpenAI)
		}
	})
}

func TestParseAnthropic(t *testing.T) {
	t.Run("full_fields", func(t *testing.T) {
		// Real Anthropic Messages API non-streaming response shape.
		body := []byte(`{
			"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant",
			"model":"claude-opus-5","content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":250,"output_tokens":40,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}
		}`)
		u := ParseAnthropic(body)
		if u.Prompt != 250 {
			t.Errorf("Prompt = %d, want 250 (input_tokens)", u.Prompt)
		}
		if u.Completion != 40 {
			t.Errorf("Completion = %d, want 40 (output_tokens)", u.Completion)
		}
		if u.Cached != 200 {
			t.Errorf("Cached = %d, want 200 (cache_read_input_tokens)", u.Cached)
		}
		if u.CacheWrite != 50 {
			t.Errorf("CacheWrite = %d, want 50 (cache_creation_input_tokens)", u.CacheWrite)
		}
		// Anthropic never sends total_tokens → Prompt+Completion.
		if u.Total != 290 {
			t.Errorf("Total = %d, want 290 (input+output)", u.Total)
		}
		if u.Source != SourceAnthropic {
			t.Errorf("Source = %q, want %q", u.Source, SourceAnthropic)
		}
	})

	t.Run("total_uses_upstream_value_when_present", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":99}}`)
		u := ParseAnthropic(body)
		if u.Total != 99 {
			t.Errorf("Total = %d, want 99 (upstream total wins)", u.Total)
		}
	})

	t.Run("no_math_on_cached_and_cache_write", func(t *testing.T) {
		// cache_read_input_tokens (200) and cache_creation_input_tokens
		// (50) are both subsets of input_tokens (300) upstream. The parser
		// must record Prompt as-is (300), never 300-200 or 300-200-50.
		body := []byte(`{"usage":{"input_tokens":300,"output_tokens":40,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}}`)
		u := ParseAnthropic(body)
		if u.Prompt != 300 {
			t.Errorf("Prompt = %d, want 300 (cached subsets must not be subtracted)", u.Prompt)
		}
		if u.Cached != 200 {
			t.Errorf("Cached = %d, want 200", u.Cached)
		}
		if u.CacheWrite != 50 {
			t.Errorf("CacheWrite = %d, want 50", u.CacheWrite)
		}
		if u.Total != 340 {
			t.Errorf("Total = %d, want 340 (300+40)", u.Total)
		}
	})

	t.Run("empty_and_garbage", func(t *testing.T) {
		for _, body := range [][]byte{nil, {}, []byte("not json"), []byte(`{"usage":null}`), []byte(`{}`)} {
			u := ParseAnthropic(body)
			if u != (Usage{}) {
				t.Errorf("ParseAnthropic(%q) = %+v, want zero Usage", body, u)
			}
		}
	})
}

func TestParseOpenAI_DoesNotReadAnthropicFields(t *testing.T) {
	// Regression for the original bug shape: an Anthropic body fed to the
	// OpenAI parser must yield zeros (prompt_tokens/completion_tokens do
	// not exist there), not garbage. The proxy selects the right parser by
	// upstream path; this test pins what happens on mis-selection.
	body := []byte(`{"usage":{"input_tokens":250,"output_tokens":40,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}}`)
	u := ParseOpenAI(body)
	if u != (Usage{}) {
		t.Errorf("ParseOpenAI on Anthropic body = %+v, want zero Usage", u)
	}
}

// TestParseOpenAI_CachedClampedToPrompt guards the cached ⊆ prompt invariant
// of the OpenAI wire format: a broken upstream report with cached tokens
// exceeding prompt_tokens is clamped to prompt so cost accounting can never
// go negative.
func TestParseOpenAI_CachedClampedToPrompt(t *testing.T) {
	// prompt_cache_hit_tokens > prompt_tokens
	u := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":100}}`))
	if u.Cached != 10 {
		t.Errorf("cached = %d, want 10 (clamped to prompt)", u.Cached)
	}
	if u.Prompt != 10 {
		t.Errorf("prompt = %d, want 10", u.Prompt)
	}

	// negative cached clamped to 0
	u2 := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":-3}}`))
	if u2.Cached != 0 {
		t.Errorf("cached = %d, want 0 (negative clamped)", u2.Cached)
	}

	// well-formed payload unchanged (as-is recording)
	u3 := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_cache_hit_tokens":30}}`))
	if u3.Cached != 30 || u3.Prompt != 100 {
		t.Errorf("well-formed: cached=%d prompt=%d, want 30/100 (record as-is)", u3.Cached, u3.Prompt)
	}
}

// TestParseOpenAI_NegativeTokensClamped guards the entry clamp: a broken
// upstream report with negative token counts is normalized to 0 (never a
// negative cost input). All-negative payloads collapse to the zero Usage
// (same as an empty payload); mixed payloads keep only the positive counts.
func TestParseOpenAI_NegativeTokensClamped(t *testing.T) {
	u := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":-10,"completion_tokens":-5,"total_tokens":-15,"prompt_cache_hit_tokens":-3}}`))
	if u != (Usage{}) {
		t.Errorf("all-negative OpenAI payload = %+v, want zero Usage (clamped to zero)", u)
	}

	u2 := ParseOpenAI([]byte(`{"usage":{"prompt_tokens":-10,"completion_tokens":50,"total_tokens":40,"prompt_cache_hit_tokens":-3}}`))
	if u2.Prompt != 0 || u2.Completion != 50 || u2.Cached != 0 || u2.Total != 40 {
		t.Errorf("mixed OpenAI payload = %+v, want Prompt=0 Completion=50 Cached=0 Total=40 (negatives clamped)", u2)
	}
	if u2.Source != SourceOpenAI {
		t.Errorf("mixed payload Source = %q, want openai (positive counts keep it a real record)", u2.Source)
	}
}

// TestParseAnthropic_NegativeTokensClamped guards the same entry clamp for
// the Anthropic form: negatives normalized to 0, formula semantics intact
// (nothing subtracted, only negatives dropped).
func TestParseAnthropic_NegativeTokensClamped(t *testing.T) {
	u := ParseAnthropic([]byte(`{"usage":{"input_tokens":-10,"output_tokens":-5,"cache_read_input_tokens":-3,"cache_creation_input_tokens":-2}}`))
	if u != (Usage{}) {
		t.Errorf("all-negative Anthropic payload = %+v, want zero Usage (clamped to zero)", u)
	}

	u2 := ParseAnthropic([]byte(`{"usage":{"input_tokens":-10,"output_tokens":40,"cache_read_input_tokens":-3,"cache_creation_input_tokens":-2}}`))
	if u2.Prompt != 0 || u2.Completion != 40 || u2.Cached != 0 || u2.CacheWrite != 0 || u2.Total != 40 {
		t.Errorf("mixed Anthropic payload = %+v, want Prompt=0 Completion=40 Cached=0 CacheWrite=0 Total=40 (negatives clamped, total=prompt+completion)", u2)
	}
	if u2.Source != SourceAnthropic {
		t.Errorf("mixed payload Source = %q, want anthropic", u2.Source)
	}
}

// TestHasTokens pins item 13: HasTokens is the single "is this usage
// non-empty" gate — a usage carrying ONLY cache tokens reports true, a
// fully zero usage reports false.
func TestHasTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  bool
	}{
		{"zero", Usage{}, false},
		{"prompt only", Usage{Prompt: 1}, true},
		{"completion only", Usage{Completion: 1}, true},
		{"cache only", Usage{Cached: 50}, true},
		{"cache write only", Usage{CacheWrite: 50}, true},
		{"reasoning only is still completion", Usage{Reasoning: 1, Completion: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.HasTokens(); got != tc.want {
				t.Errorf("HasTokens(%+v) = %v, want %v", tc.usage, got, tc.want)
			}
		})
	}
}

// TestParseAnthropic_CacheOnlySurvives pins item 13 at the parser level: an
// Anthropic payload with ONLY cache_read_input_tokens is a real usage record
// (cache-only hit) and must not be discarded as empty.
func TestParseAnthropic_CacheOnlySurvives(t *testing.T) {
	u := ParseAnthropic([]byte(`{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}`))
	if u.Cached != 50 || u.Prompt != 0 || u.Completion != 0 {
		t.Errorf("cache-only Anthropic payload = %+v, want Cached=50 Prompt=0 Completion=0", u)
	}
	if !u.HasTokens() {
		t.Error("cache-only usage must report HasTokens()=true")
	}
	if u.Source != SourceAnthropic {
		t.Errorf("Source = %q, want anthropic", u.Source)
	}
}
