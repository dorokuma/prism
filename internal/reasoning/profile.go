package reasoning

import "strings"

// Form describes the reasoning control morphology of an upstream model.
type Form string

const (
	FormNone   Form = "none"
	FormEnum   Form = "enum"
	FormToggle Form = "toggle"
	FormBudget Form = "budget"
)

// Effort schema identifiers select which profile table a model is mapped
// against. They are derived from the upstream provider's base_url host
// (see config.buildProviderSchema) and carry no new YAML configuration.
const (
	// SchemaOpencode is the default opencode-go schema (empty string).
	SchemaOpencode = ""
	// SchemaOllama selects the ollama-cloud profile table.
	SchemaOllama = "ollama"
)

// Profile holds the reasoning-effort mapping rules for one upstream model
// (or a family sharing the same prefix).
type Profile struct {
	Form Form

	// FormEnum only — abstract-level → real-value lookup, e.g. {"low":"low","medium":"medium","high":"high","xhigh":"xhigh"}.
	EffortMap map[string]string

	// FormEnum only — dot-path fields to write the real value into, e.g. ["reasoning_effort"].
	EnumFields []string

	// FormBudget only — abstract-level → token budget.
	BudgetMap map[string]int

	// FormBudget only — cap for xhigh (also used as budget for xhigh).
	BudgetMax int

	// Dot-path for the toggle/switch field, e.g. "thinking.type" or "thinking".
	ToggleField string

	// Value to write when turning thinking on (for ToggleField).
	ToggleOn string

	// Value to write when turning thinking off (for ToggleField).
	ToggleOff string

	// FormEnum only — when set, an abstract "off" level writes this value into
	// EnumFields instead of deleting them + toggling off. Empty means the
	// opencode behaviour (delete enum fields + set ToggleOff). Used by the
	// ollama schema where thinking cannot be disabled via field deletion.
	OffValue string

	// FormToggle only — delete reasoning_effort and thinking.level from the request body.
	StripEnumOnToggle bool

	// When true the model cannot disable thinking; off → forced on + Warn.
	ForceOn bool

	// When true run the two-stage DeepSeek compatibility branch (uses util.MapThoughtLevel,
	// does not touch toggle, does not delete fields).
	DeepSeekCompat bool
}

type namedProfile struct {
	prefixes []string
	profile  Profile
}

// builtinProfiles is ordered longest-prefix-first so that a more specific
// prefix (e.g. "glm-5.2") wins over a shorter one (e.g. "glm-5").
var builtinProfiles = []namedProfile{
	{
		prefixes: []string{"deepseek-"},
		profile: Profile{
			Form:           FormEnum,
			DeepSeekCompat: true,
			EffortMap:      map[string]string{"low": "high", "medium": "high", "high": "high", "xhigh": "max"},
			EnumFields:     []string{"reasoning_effort", "thinking.level"},
		},
	},
	{
		prefixes: []string{"glm-5.2"},
		profile: Profile{
			Form:        FormEnum,
			EffortMap:   map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh"},
			EnumFields:  []string{"reasoning_effort"},
			ToggleField: "thinking.type",
			ToggleOn:    "enabled",
			ToggleOff:   "disabled",
		},
	},
	{
		prefixes: []string{"glm-5.1"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"glm-5"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"kimi-k3"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "low", "high": "high", "xhigh": "max"},
			EnumFields: []string{"reasoning_effort"},
		},
	},
	{
		prefixes: []string{"kimi-k2.7-code"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			StripEnumOnToggle: true,
			ForceOn:           true,
		},
	},
	{
		prefixes: []string{"kimi-k2.5"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"kimi-k2.6"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"hy3"},
		profile: Profile{
			Form:        FormEnum,
			EffortMap:   map[string]string{"low": "low", "medium": "low", "high": "high", "xhigh": "high"},
			EnumFields:  []string{"reasoning_effort"},
			ToggleField: "thinking.type",
			ToggleOn:    "enabled",
			ToggleOff:   "disabled",
		},
	},
	{
		prefixes: []string{"mimo-"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking.type",
			ToggleOn:          "enabled",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"minimax-m2.5"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking",
			ToggleOn:          "adaptive",
			StripEnumOnToggle: true,
			ForceOn:           true,
		},
	},
	{
		prefixes: []string{"minimax-m2.7"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking",
			ToggleOn:          "adaptive",
			StripEnumOnToggle: true,
			ForceOn:           true,
		},
	},
	{
		prefixes: []string{"minimax-m3"},
		profile: Profile{
			Form:              FormToggle,
			ToggleField:       "thinking",
			ToggleOn:          "adaptive",
			ToggleOff:         "disabled",
			StripEnumOnToggle: true,
		},
	},
	{
		prefixes: []string{"qwen3.7-plus"},
		profile: Profile{
			Form:      FormBudget,
			BudgetMap: map[string]int{"low": 4096, "medium": 16384, "high": 32768, "xhigh": 81920},
			BudgetMax: 81920,
		},
	},
	{
		prefixes: []string{"qwen3.7-max"},
		profile: Profile{
			Form:      FormBudget,
			BudgetMap: map[string]int{"low": 4096, "medium": 16384, "high": 32768, "xhigh": 81920},
			BudgetMax: 81920,
		},
	},
	{
		prefixes: []string{"qwen3.6-plus"},
		profile: Profile{
			Form:      FormBudget,
			BudgetMap: map[string]int{"low": 4096, "medium": 16384, "high": 32768, "xhigh": 81920},
			BudgetMax: 81920,
		},
	},
	{
		prefixes: []string{"qwen3.5-plus"},
		profile: Profile{
			Form:      FormBudget,
			BudgetMap: map[string]int{"low": 4096, "medium": 16384, "high": 32768, "xhigh": 81920},
			BudgetMax: 81920,
		},
	},
	{
		// Longest prefixes first is not required (matchPrefix picks the
		// longest hit) but keep SKU-specific grok-4.20 names explicit so a
		// future grok-4.20-* family prefix cannot swallow non-reasoning.
		prefixes: []string{"grok-4.20-0309-reasoning"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "none",
		},
	},
	{
		prefixes: []string{"grok-4.20-multi-agent-0309"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh"},
			EnumFields: []string{"reasoning_effort"},
		},
	},
	{
		prefixes: []string{"grok-4.6"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh"},
			EnumFields: []string{"reasoning_effort"},
			ForceOn:    true,
		},
	},
	{
		prefixes: []string{"grok-4.5"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "high"},
			EnumFields: []string{"reasoning_effort"},
			ForceOn:    true,
		},
	},
	{
		prefixes: []string{"grok-4.3"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "none",
		},
	},
	{
		prefixes: []string{"grok-build-0.1"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"high": "high"},
			EnumFields: []string{"reasoning_effort"},
			ForceOn:    true,
		},
	},
}

// ollamaProfiles maps ollama-cloud upstream models against their real
// thinking levels. All profiles use FormEnum with EnumFields=["reasoning_effort"]
// and no ToggleField (ollama does not use thinking.type). OffValue (set when a
// model exposes a "none"/disabled level) is written instead of deleting the
// field; ForceOn forces the minimum real level + Warn when the model cannot
// disable thinking at all.
var ollamaProfiles = []namedProfile{
	{
		prefixes: []string{"gpt-oss"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "high"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "low",
			ForceOn:    true,
		},
	},
	{
		prefixes: []string{"glm-5.2"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "high", "medium": "high", "high": "high", "xhigh": "max"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "none",
		},
	},
	{
		prefixes: []string{"qwen3-vl"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "max"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "low",
			ForceOn:    true,
		},
	},
	{
		prefixes: []string{"qwen3"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "medium", "medium": "medium", "high": "medium", "xhigh": "medium"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "none",
		},
	},
	{
		prefixes: []string{"kimi-k2-thinking"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "max"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "low",
			ForceOn:    true,
		},
	},
	{
		prefixes: []string{"minimax"},
		profile: Profile{
			Form:       FormEnum,
			EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "max"},
			EnumFields: []string{"reasoning_effort"},
			OffValue:   "low",
			ForceOn:    true,
		},
	},
}

// ollamaDefaultProfile is the fallback for ollama-schema models that do not
// match any explicit ollama prefix. It covers deepseek-v4-pro/hy3/mimo and any
// unlisted model. EffortMap 1:1, no thinking.type, off→none (set value).
var ollamaDefaultProfile = Profile{
	Form:       FormEnum,
	EffortMap:  map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "max"},
	EnumFields: []string{"reasoning_effort"},
	OffValue:   "none",
}

// ProfileFor returns the Profile for the given upstream model name, using the
// profile table selected by schema. schema == SchemaOllama selects the
// ollama-cloud table (with ollamaDefaultProfile fallback); any other value
// (including the empty opencode schema) uses the built-in opencode table
// (with a FormNone fallback).
func matchPrefix(profiles []namedProfile, model string) (Profile, bool) {
	m := strings.ToLower(model)

	var best *Profile
	bestLen := 0

	for _, np := range profiles {
		for _, prefix := range np.prefixes {
			if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
				best = &np.profile
				bestLen = len(prefix)
			}
		}
	}

	if best != nil {
		return *best, true
	}
	return Profile{}, false
}

// ProfileFor returns the Profile for the given upstream model name, using the
// profile table selected by schema. schema == SchemaOllama selects the
// ollama-cloud table (with ollamaDefaultProfile fallback); any other value
// (including the empty opencode schema) uses the built-in opencode table
// (with a FormNone fallback).
func ProfileFor(model, schema string) Profile {
	if schema == SchemaOllama {
		if p, ok := matchPrefix(ollamaProfiles, model); ok {
			return p
		}
		return ollamaDefaultProfile
	}
	if p, ok := matchPrefix(builtinProfiles, model); ok {
		return p
	}
	return Profile{Form: FormNone}
}

// ProfileForModel is a convenience wrapper around ProfileFor with the default
// opencode schema. Kept for backward compatibility with existing callers/tests.
func ProfileForModel(model string) Profile {
	return ProfileFor(model, SchemaOpencode)
}
