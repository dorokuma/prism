package dsml

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolCall is one recovered function call in OpenAI chat-completions shape.
type ToolCall struct {
	Name      string
	Arguments string // JSON object text
}

var (
	tagOpenRe     = regexp.MustCompile(`(?i)<(?:｜{1,2}DSML｜{1,2}|DSML｜|\s*\|\s*DSML\s*\|\s*)`)
	tagCloseRe    = regexp.MustCompile(`(?i)</(?:｜{1,2}DSML｜{1,2}|DSML｜|\s*\|\s*DSML\s*\|\s*)`)
	invokeRe      = regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(dsmlToken) + `invoke\s+name="([^"]*)"\s*>`)
	paramRe       = regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(dsmlToken) + `parameter\s+name="([^"]+)"(?:\s+string="(true|false)")?\s*>(.*?)</` + regexp.QuoteMeta(dsmlToken) + `parameter>`)
	invokeCloseRe = regexp.MustCompile(`(?i)</` + regexp.QuoteMeta(dsmlToken) + `invoke>`)
)

// RecoverInvokes tries to assemble complete tool calls from buffered DSML
// text using DeepSeek encoding_dsv4.py parse_tool_calls semantics. Name must
// be non-empty and arguments must be assemblable. Incomplete leaks (half a
// tag plus chain-of-thought) return ok=false.
func RecoverInvokes(text string) (calls []ToolCall, ok bool) {
	canon := canonicalizeTags(text)
	if c, err := parseOfficial(canon); err == nil && validCalls(c) {
		return c, true
	}
	if c := parseLenient(canon); validCalls(c) {
		return c, true
	}
	return nil, false
}

func validCalls(calls []ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if strings.TrimSpace(c.Name) == "" {
			return false
		}
		if !json.Valid([]byte(c.Arguments)) {
			return false
		}
	}
	return true
}

func canonicalizeTags(s string) string {
	s = tagCloseRe.ReplaceAllString(s, "</"+dsmlToken)
	s = tagOpenRe.ReplaceAllString(s, "<"+dsmlToken)
	return s
}

func parseOfficial(text string) ([]ToolCall, error) {
	start := "<" + dsmlToken + "tool_calls"
	idx := strings.Index(text, start)
	if idx < 0 {
		start = "<" + dsmlToken + "function_calls"
		idx = strings.Index(text, start)
		if idx < 0 {
			return nil, fmt.Errorf("no tool_calls block")
		}
	}
	rest := text[idx+len(start):]
	return parseToolCalls(rest)
}

// parseToolCalls ports encoding_dsv4.py parse_tool_calls from the position
// immediately after `<｜DSML｜tool_calls`.
func parseToolCalls(text string) ([]ToolCall, error) {
	endTok := "</" + dsmlToken + "tool_calls>"
	altEnd := "</" + dsmlToken + "function_calls>"
	invokeTok := "<" + dsmlToken + "invoke"
	paramTok := "<" + dsmlToken + "parameter"
	invokeEndTok := "</" + dsmlToken + "invoke"
	paramStop := "/" + dsmlToken + "parameter"

	var calls []ToolCall
	index := 0
	for index < len(text) {
		content, stop, newIdx := readUntilStop(text, index, []string{invokeTok, endTok, altEnd})
		index = newIdx
		if content != ">\n" {
			return nil, fmt.Errorf("tool call format error: expected '>\\n' got %q", content)
		}
		if stop == endTok || stop == altEnd {
			break
		}
		if stop == "" {
			return nil, fmt.Errorf("missing special token in tool calls")
		}

		toolNameContent, stop, newIdx := readUntilStop(text, index, []string{paramTok, invokeEndTok})
		index = newIdx
		name, err := parseToolName(toolNameContent)
		if err != nil {
			return nil, err
		}

		args := make([][3]string, 0) // name, value, isString
		seen := map[string]struct{}{}
		for stop == paramTok {
			paramContent, _, newIdx := readUntilStop(text, index, []string{paramStop})
			index = newIdx
			pname, isStr, pval, err := parseParam(paramContent)
			if err != nil {
				return nil, err
			}
			if _, dup := seen[pname]; dup {
				return nil, fmt.Errorf("duplicate parameter name: %s", pname)
			}
			seen[pname] = struct{}{}
			args = append(args, [3]string{pname, pval, isStr})

			var next string
			next, stop, index = readUntilStop(text, index, []string{paramTok, invokeEndTok})
			if next != ">\n" {
				return nil, fmt.Errorf("parameter format error: expected '>\\n' got %q", next)
			}
		}
		calls = append(calls, ToolCall{Name: name, Arguments: encodeArguments(args)})
	}
	return calls, nil
}

func parseToolName(toolNameContent string) (string, error) {
	// encoding_dsv4: r'^\s*name="(.*?)">\n$'
	s := toolNameContent
	if !strings.HasSuffix(s, ">\n") {
		return "", fmt.Errorf("tool name format error: %q", toolNameContent)
	}
	s = strings.TrimPrefix(s, " ")
	s = strings.TrimLeft(s, "\t")
	// leftover leading whitespace
	s = strings.TrimLeft(s, " \t")
	const prefix = `name="`
	if !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("tool name format error: %q", toolNameContent)
	}
	s = s[len(prefix):]
	end := strings.Index(s, `">`+"\n")
	if end < 0 {
		return "", fmt.Errorf("tool name format error: %q", toolNameContent)
	}
	name := s[:end]
	if name == "" {
		return "", fmt.Errorf("empty tool name")
	}
	return name, nil
}

func parseParam(paramContent string) (name, isStr, value string, err error) {
	// encoding_dsv4: r'^ name="(.*?)" string="(true|false)">(.*?)<$'
	s := paramContent
	if !strings.HasPrefix(s, ` name="`) || !strings.HasSuffix(s, "<") {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	s = strings.TrimPrefix(s, ` name="`)
	s = strings.TrimSuffix(s, "<")
	q := strings.Index(s, `"`)
	if q < 0 {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	name = s[:q]
	s = s[q+1:]
	const mid = ` string="`
	if !strings.HasPrefix(s, mid) {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	s = s[len(mid):]
	q = strings.Index(s, `"`)
	if q < 0 {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	isStr = s[:q]
	if isStr != "true" && isStr != "false" {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	s = s[q+1:]
	if !strings.HasPrefix(s, ">") {
		return "", "", "", fmt.Errorf("parameter format error: %q", paramContent)
	}
	value = s[1:]
	if name == "" {
		return "", "", "", fmt.Errorf("empty parameter name")
	}
	return name, isStr, value, nil
}

func encodeArguments(args [][3]string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		keyJSON, _ := json.Marshal(a[0])
		val := a[1]
		if a[2] == "true" {
			b, err := json.Marshal(val)
			if err != nil {
				b, _ = json.Marshal(val)
			}
			val = string(b)
		}
		parts = append(parts, string(keyJSON)+": "+val)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func readUntilStop(text string, index int, stop []string) (content, matched string, newIndex int) {
	minPos := len(text)
	matched = ""
	for _, s := range stop {
		pos := strings.Index(text[index:], s)
		if pos != -1 {
			abs := index + pos
			if abs < minPos {
				minPos = abs
				matched = s
			}
		}
	}
	if matched != "" {
		return text[index:minPos], matched, minPos + len(matched)
	}
	return text[index:], "", len(text)
}

func parseLenient(text string) []ToolCall {
	var calls []ToolCall
	for _, loc := range invokeRe.FindAllStringSubmatchIndex(text, -1) {
		if len(loc) < 4 {
			continue
		}
		name := strings.TrimSpace(text[loc[2]:loc[3]])
		if name == "" {
			continue
		}
		bodyStart := loc[1]
		closeLoc := invokeCloseRe.FindStringIndex(text[bodyStart:])
		if closeLoc == nil {
			continue
		}
		body := text[bodyStart : bodyStart+closeLoc[0]]
		args := parseLenientArgs(body)
		if args == "" {
			continue
		}
		calls = append(calls, ToolCall{Name: name, Arguments: args})
	}
	return calls
}

func parseLenientArgs(body string) string {
	matches := paramRe.FindAllStringSubmatch(body, -1)
	if len(matches) > 0 {
		var args [][3]string
		seen := map[string]struct{}{}
		for _, m := range matches {
			name := strings.TrimSpace(m[1])
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			isStr := m[2]
			if isStr == "" {
				isStr = "true"
			}
			args = append(args, [3]string{name, m[3], isStr})
		}
		if len(args) > 0 {
			return encodeArguments(args)
		}
	}
	trimmed := strings.TrimSpace(body)
	if json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	return ""
}

func removedNotice(n int) string {
	return fmt.Sprintf("[prism] removed %d chars of aborted DSML output", n)
}
