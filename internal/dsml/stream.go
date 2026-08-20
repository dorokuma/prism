package dsml

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

const (
	modePass  = 0
	modeGuard = 1
)

// Rewriter is a per-response state machine for OpenAI-style chat SSE.
// Passthrough keeps original event bytes. Guard mode intercepts content.
type Rewriter struct {
	mode        int
	tail        string
	held        [][]byte
	heldContent []byte
	buf         strings.Builder
	meta        chunkMeta
	heldTerm    [][]byte
	sawDone     bool
}

type chunkMeta struct {
	ID      string
	Object  string
	Created json.RawMessage
	Model   string
	Finger  string
}

// RewriteStream applies the guard to a complete SSE body.
func RewriteStream(in []byte) []byte {
	r := NewRewriter()
	var out []byte
	rest := in
	for {
		ev, next, ok := splitSSEEvent(rest)
		if !ok {
			break
		}
		out = append(out, r.PushEvent(ev)...)
		rest = next
	}
	if len(rest) > 0 {
		out = append(out, r.PushEvent(rest)...)
	}
	out = append(out, r.Finish()...)
	return out
}

func NewRewriter() *Rewriter { return &Rewriter{} }

func splitSSEEvent(buf []byte) (event, rest []byte, ok bool) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return nil, buf, false
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return buf[:crlf+4], buf[crlf+4:], true
	default:
		return buf[:lf+2], buf[lf+2:], true
	}
}

// PushEvent consumes one SSE event (including its trailing blank line when
// present) and returns bytes that are safe to write now.
func (r *Rewriter) PushEvent(ev []byte) []byte {
	payload, done, hasData := sseData(ev)
	if done {
		r.sawDone = true
		if r.mode == modeGuard {
			r.heldTerm = append(r.heldTerm, ev)
			return nil
		}
		return concat(r.flushHeldOriginal(), ev)
	}
	if !hasData {
		if r.mode == modeGuard {
			return ev
		}
		return ev
	}
	if len(payload) == 0 || payload[0] != '{' {
		if r.mode == modeGuard {
			return ev
		}
		return concat(r.flushHeldOriginal(), ev)
	}
	return r.pushJSON(ev, payload)
}

func (r *Rewriter) Finish() []byte {
	if r.mode == modePass {
		return r.flushHeldOriginal()
	}
	return r.finishGuard()
}

func (r *Rewriter) pushJSON(ev, payload []byte) []byte {
	info, err := inspectChunk(payload)
	if err != nil {
		if r.mode == modeGuard {
			return ev
		}
		return concat(r.flushHeldOriginal(), ev)
	}
	r.noteMeta(info)
	content := info.content
	hasContent := info.hasContent
	finish := info.finish

	if r.mode == modeGuard {
		return r.guardEvent(ev, payload, content, hasContent, finish, info)
	}

	if !hasContent {
		if finish != "" {
			return concat(r.flushHeldOriginal(), ev)
		}
		return ev
	}

	combined := r.tail + string(content)
	if HasMarker(string(r.heldContent)+string(content)) || HasMarker(combined) {
		r.enterGuard()
		var out []byte
		for _, h := range r.held {
			out = append(out, r.guardRaw(h)...)
		}
		r.held = nil
		r.heldContent = nil
		out = append(out, r.guardEvent(ev, payload, content, true, finish, info)...)
		return out
	}
	h := holdLen(combined)
	if h > 0 {
		r.held = append(r.held, ev)
		r.heldContent = append(r.heldContent, content...)
		r.tail = combined[len(combined)-h:]
		return nil
	}
	out := concat(r.flushHeldOriginal(), ev)
	if len(combined) > maxHoldBytes {
		r.tail = combined[len(combined)-maxHoldBytes:]
	} else {
		r.tail = combined
	}
	return out
}

func (r *Rewriter) enterGuard() {
	r.mode = modeGuard
	r.tail = ""
}

func (r *Rewriter) guardRaw(ev []byte) []byte {
	payload, done, hasData := sseData(ev)
	if done {
		r.sawDone = true
		r.heldTerm = append(r.heldTerm, ev)
		return nil
	}
	if !hasData || len(payload) == 0 || payload[0] != '{' {
		return ev
	}
	info, err := inspectChunk(payload)
	if err != nil {
		return ev
	}
	r.noteMeta(info)
	return r.guardEvent(ev, payload, info.content, info.hasContent, info.finish, info)
}

func (r *Rewriter) guardEvent(ev, payload, content []byte, hasContent bool, finish string, info chunkInfo) []byte {
	if hasContent {
		r.buf.Write(content)
	}
	if finish != "" {
		r.heldTerm = append(r.heldTerm, ev)
		return nil
	}
	if info.hasUsage && !hasContent {
		return ev
	}
	stripped, keep := stripDeltaContent(payload)
	if !keep {
		return nil
	}
	return encodeSSEData(stripped, sseEOL(ev))
}

func (r *Rewriter) flushHeldOriginal() []byte {
	if len(r.held) == 0 {
		return nil
	}
	var out []byte
	for _, h := range r.held {
		out = append(out, h...)
	}
	r.held = nil
	r.heldContent = nil
	return out
}

func (r *Rewriter) finishGuard() []byte {
	text := r.buf.String()
	n := len(text)
	calls, recovered := RecoverInvokes(text)

	var b bytes.Buffer
	if recovered {
		b.Write(r.emitToolCalls(calls))
	} else {
		b.Write(r.emitContent(removedNotice(n)))
	}

	eol := "\n\n"
	emittedTerm := false
	for _, ev := range r.heldTerm {
		_, done, _ := sseData(ev)
		if done {
			continue
		}
		payload, _, _ := sseData(ev)
		if recovered {
			rewritten, err := setFinishReason(payload, "tool_calls")
			if err != nil {
				b.Write(ev)
			} else {
				rewritten, _ = stripMessageContent(rewritten)
				b.Write(encodeSSEData(rewritten, sseEOL(ev)))
			}
		} else {
			rewritten, err := stripMessageContent(payload)
			if err != nil {
				b.Write(ev)
			} else {
				b.Write(encodeSSEData(rewritten, sseEOL(ev)))
			}
		}
		emittedTerm = true
		eol = sseEOL(ev)
	}
	if !emittedTerm {
		reason := ""
		if recovered {
			reason = "tool_calls"
		}
		b.Write(r.emitFinish(reason, eol))
	}
	if r.sawDone {
		b.WriteString("data: [DONE]")
		b.WriteString(eol)
	}
	return b.Bytes()
}

func (r *Rewriter) noteMeta(info chunkInfo) {
	if info.ID != "" {
		r.meta.ID = info.ID
	}
	if info.Object != "" {
		r.meta.Object = info.Object
	}
	if len(info.Created) > 0 {
		r.meta.Created = info.Created
	}
	if info.Model != "" {
		r.meta.Model = info.Model
	}
	if info.Finger != "" {
		r.meta.Finger = info.Finger
	}
}

func (r *Rewriter) emitToolCalls(calls []ToolCall) []byte {
	type fn struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type tc struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}
	items := make([]tc, 0, len(calls))
	for i, c := range calls {
		items = append(items, tc{
			Index: i,
			ID:    newCallID(),
			Type:  "function",
			Function: fn{
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		})
	}
	chunk := r.baseChunk()
	chunk["choices"] = mustJSON([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{"tool_calls": items},
		"finish_reason": nil,
	}})
	return encodeSSEData(mustJSON(chunk), "\n\n")
}

func (r *Rewriter) emitContent(s string) []byte {
	chunk := r.baseChunk()
	chunk["choices"] = mustJSON([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{"content": s},
		"finish_reason": nil,
	}})
	return encodeSSEData(mustJSON(chunk), "\n\n")
}

func (r *Rewriter) emitFinish(reason, eol string) []byte {
	chunk := r.baseChunk()
	var fr any
	if reason != "" {
		fr = reason
	}
	chunk["choices"] = mustJSON([]map[string]any{{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": fr,
	}})
	if eol == "" {
		eol = "\n\n"
	}
	return encodeSSEData(mustJSON(chunk), eol)
}

func (r *Rewriter) baseChunk() map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if r.meta.ID != "" {
		m["id"] = mustJSON(r.meta.ID)
	}
	obj := r.meta.Object
	if obj == "" {
		obj = "chat.completion.chunk"
	}
	m["object"] = mustJSON(obj)
	if len(r.meta.Created) > 0 {
		m["created"] = r.meta.Created
	}
	if r.meta.Model != "" {
		m["model"] = mustJSON(r.meta.Model)
	}
	if r.meta.Finger != "" {
		m["system_fingerprint"] = mustJSON(r.meta.Finger)
	}
	return m
}

func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

type chunkInfo struct {
	ID         string
	Object     string
	Created    json.RawMessage
	Model      string
	Finger     string
	content    []byte
	hasContent bool
	finish     string
	hasUsage   bool
}

func inspectChunk(payload []byte) (chunkInfo, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return chunkInfo{}, err
	}
	info := chunkInfo{
		Created:  raw["created"],
		hasUsage: len(raw["usage"]) > 0 && string(raw["usage"]) != "null",
	}
	_ = json.Unmarshal(raw["id"], &info.ID)
	_ = json.Unmarshal(raw["object"], &info.Object)
	_ = json.Unmarshal(raw["model"], &info.Model)
	_ = json.Unmarshal(raw["system_fingerprint"], &info.Finger)

	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil || len(choices) == 0 {
		return info, nil
	}
	ch := choices[0]
	if fr := ch["finish_reason"]; len(fr) > 0 && string(fr) != "null" {
		_ = json.Unmarshal(fr, &info.finish)
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(ch["delta"], &delta); err != nil {
		return info, nil
	}
	if c, ok := delta["content"]; ok && len(c) > 0 && string(c) != "null" {
		if b, ok := decodeJSONString(c); ok {
			info.content = b
			info.hasContent = true
		}
	}
	return info, nil
}

func stripDeltaContent(payload []byte) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return payload, true
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil || len(choices) == 0 {
		return payload, true
	}
	ch := choices[0]
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(ch["delta"], &delta); err != nil {
		return payload, true
	}
	delete(delta, "content")
	if !deltaInteresting(delta) && (len(ch["finish_reason"]) == 0 || string(ch["finish_reason"]) == "null") &&
		(len(raw["usage"]) == 0 || string(raw["usage"]) == "null") {
		return nil, false
	}
	ch["delta"] = mustJSON(delta)
	choices[0] = ch
	raw["choices"] = mustJSON(choices)
	return mustJSON(raw), true
}

func stripMessageContent(payload []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return payload, err
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil || len(choices) == 0 {
		return payload, nil
	}
	ch := choices[0]
	if d := ch["delta"]; len(d) > 0 {
		var delta map[string]json.RawMessage
		if json.Unmarshal(d, &delta) == nil {
			delete(delta, "content")
			ch["delta"] = mustJSON(delta)
		}
	}
	choices[0] = ch
	raw["choices"] = mustJSON(choices)
	return mustJSON(raw), nil
}

func setFinishReason(payload []byte, reason string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return payload, err
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil || len(choices) == 0 {
		return payload, nil
	}
	choices[0]["finish_reason"] = mustJSON(reason)
	if d := choices[0]["delta"]; len(d) > 0 {
		var delta map[string]json.RawMessage
		if json.Unmarshal(d, &delta) == nil {
			delete(delta, "content")
			choices[0]["delta"] = mustJSON(delta)
		}
	}
	raw["choices"] = mustJSON(choices)
	return mustJSON(raw), nil
}

func deltaInteresting(delta map[string]json.RawMessage) bool {
	for k, v := range delta {
		if k == "content" {
			continue
		}
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		s := strings.TrimSpace(string(v))
		if s == "{}" || s == "[]" || s == `""` {
			continue
		}
		return true
	}
	return false
}

func sseData(ev []byte) (payload []byte, done, hasData bool) {
	norm := bytes.ReplaceAll(ev, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	var data [][]byte
	for _, line := range bytes.Split(norm, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			v := bytes.TrimSpace(line[len("data:"):])
			data = append(data, v)
			hasData = true
		}
	}
	if !hasData {
		return nil, false, false
	}
	payload = bytes.Join(data, []byte("\n"))
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return payload, true, true
	}
	return payload, false, true
}

func sseEOL(ev []byte) string {
	if bytes.HasSuffix(ev, []byte("\r\n\r\n")) {
		return "\r\n\r\n"
	}
	return "\n\n"
}

func encodeSSEData(payload []byte, eol string) []byte {
	if len(payload) == 0 {
		return nil
	}
	if eol == "" {
		eol = "\n\n"
	}
	out := make([]byte, 0, 6+len(payload)+len(eol))
	out = append(out, []byte("data: ")...)
	out = append(out, payload...)
	out = append(out, []byte(eol)...)
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func concat(a, b []byte) []byte {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]byte, len(a)+len(b))
	copy(out, a)
	copy(out[len(a):], b)
	return out
}

// GuardReader wraps an upstream SSE body and rewrites it as it is read.
type GuardReader struct {
	src      io.ReadCloser
	r        *Rewriter
	in       []byte
	out      bytes.Buffer
	eof      bool
	finished bool
	readErr  error
}

func NewGuardReader(src io.ReadCloser) *GuardReader {
	return &GuardReader{src: src, r: NewRewriter()}
}

func (g *GuardReader) Read(p []byte) (int, error) {
	for g.out.Len() == 0 && !g.finished && g.readErr == nil {
		if !g.eof {
			buf := make([]byte, 8192)
			n, err := g.src.Read(buf)
			if n > 0 {
				g.in = append(g.in, buf[:n]...)
				g.drainEvents()
			}
			if err == io.EOF {
				g.eof = true
				continue
			}
			if err != nil {
				g.readErr = err
				break
			}
			if n == 0 {
				g.eof = true
			}
			continue
		}
		if len(g.in) > 0 {
			g.out.Write(g.r.PushEvent(g.in))
			g.in = nil
		}
		g.out.Write(g.r.Finish())
		g.finished = true
	}
	if g.out.Len() > 0 {
		n, _ := g.out.Read(p)
		if g.out.Len() == 0 && g.finished {
			if g.readErr != nil {
				return n, g.readErr
			}
			return n, io.EOF
		}
		return n, nil
	}
	if g.readErr != nil {
		return 0, g.readErr
	}
	return 0, io.EOF
}

func (g *GuardReader) drainEvents() {
	for {
		ev, rest, ok := splitSSEEvent(g.in)
		if !ok {
			return
		}
		g.out.Write(g.r.PushEvent(ev))
		g.in = rest
	}
}

func (g *GuardReader) Close() error {
	if g.src != nil {
		return g.src.Close()
	}
	return nil
}
