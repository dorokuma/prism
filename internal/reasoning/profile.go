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
}

// ProfileFor returns the Profile for the given upstream model name.
// It performs a longest-prefix match against the built-in table.
// Unmatched models return a Profile with FormNone.
func ProfileFor(model string) Profile {
	m := strings.ToLower(model)

	var best *Profile
	bestLen := 0

	for _, np := range builtinProfiles {
		for _, prefix := range np.prefixes {
			if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
				best = &np.profile
				bestLen = len(prefix)
			}
		}
	}

	if best != nil {
		return *best
	}
	return Profile{Form: FormNone}
}
