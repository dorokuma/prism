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
