package agyusage

// Protobuf wire walker for agy conversation blobs. There is no published
// proto; field numbers were pinned against live antigravity-cli databases.
//
// gen_metadata.data:
//   field 1 = GeneratorMetadata
//     field 4 = usage bag (varints: 1=fresh input, 2=cache read, 3=output, 5=thinking)
//     field 19 = model string
//     field 20 = repeated kv {1: key, 2: value}; key "request_id"
// steps.metadata:
//   field 1.1 = unix seconds
//
// Quota unit (week reversal) is f1+f2+f3+f5. Cache and thinking are in
// the pool; ccusage maps the same four buckets. Field 5 is thinking
// tokens, not milliseconds (live sample: f5=40716 with 1s wall clock
// between successive gens — impossible as duration; corr(f5, dur)≈0.05).
// Unnamed bag varints stay out of the sum: f6 is always 24 (enum),
// f9/f10 have no name and would add ~0.6% of thinking if guessed in.

const (
	wireVarint = 0
	wire64     = 1
	wireBytes  = 2
	wire32     = 5
)

func consumeVarint(b []byte) (uint64, int) {
	var n uint64
	var s uint
	for i := 0; i < len(b); i++ {
		x := b[i]
		n |= uint64(x&0x7f) << s
		if x < 0x80 {
			return n, i + 1
		}
		s += 7
		if s > 63 {
			return 0, 0
		}
	}
	return 0, 0
}

// walk visits each protobuf field in b. fn returning false stops the walk.
// Truncated or invalid wire is ignored (stop); callers never panic.
func walk(b []byte, fn func(num, wt int, v uint64, payload []byte) bool) {
	i := 0
	for i < len(b) {
		key, n := consumeVarint(b[i:])
		if n == 0 {
			return
		}
		i += n
		num := int(key >> 3)
		wt := int(key & 7)
		switch wt {
		case wireVarint:
			v, n := consumeVarint(b[i:])
			if n == 0 {
				return
			}
			i += n
			if !fn(num, wt, v, nil) {
				return
			}
		case wire64:
			if i+8 > len(b) {
				return
			}
			i += 8
			if !fn(num, wt, 0, b[i-8:i]) {
				return
			}
		case wireBytes:
			ln, n := consumeVarint(b[i:])
			if n == 0 {
				return
			}
			i += n
			end := i + int(ln)
			if int(ln) < 0 || end > len(b) {
				return
			}
			payload := b[i:end]
			i = end
			if !fn(num, wt, 0, payload) {
				return
			}
		case wire32:
			if i+4 > len(b) {
				return
			}
			i += 4
			if !fn(num, wt, 0, b[i-4:i]) {
				return
			}
		default:
			return
		}
	}
}

func messageField(b []byte, num int) []byte {
	var found []byte
	walk(b, func(n, wt int, _ uint64, payload []byte) bool {
		if n == num && wt == wireBytes {
			found = payload
			return false
		}
		return true
	})
	return found
}

func varintField(b []byte, num int) (uint64, bool) {
	var v uint64
	var ok bool
	walk(b, func(n, wt int, val uint64, _ []byte) bool {
		if n == num && wt == wireVarint {
			v = val
			ok = true
			return false
		}
		return true
	})
	return v, ok
}

func stringField(b []byte, num int) string {
	payload := messageField(b, num)
	if payload == nil {
		return ""
	}
	return string(payload)
}

type tokenBag struct {
	Prompt     int64
	Cached     int64
	Completion int64
	Reasoning  int64
}

// total is the Gemini week-pool unit: every named billed-looking bucket.
func (u tokenBag) total() int64 {
	return u.Prompt + u.Cached + u.Completion + u.Reasoning
}

func parseUsageBag(b []byte) tokenBag {
	var u tokenBag
	walk(b, func(num, wt int, v uint64, _ []byte) bool {
		if wt != wireVarint {
			return true
		}
		switch num {
		case 1:
			u.Prompt = int64(v)
		case 2:
			u.Cached = int64(v)
		case 3:
			u.Completion = int64(v)
		case 5:
			u.Reasoning = int64(v)
		}
		return true
	})
	return u
}

func parseKV(b []byte) (key, value string) {
	return stringField(b, 1), stringField(b, 2)
}

func parseRequestID(gm []byte) string {
	var id string
	walk(gm, func(num, wt int, _ uint64, payload []byte) bool {
		if num != 20 || wt != wireBytes {
			return true
		}
		k, v := parseKV(payload)
		if k == "request_id" && v != "" {
			id = v
			return false
		}
		return true
	})
	return id
}

func parseModel(gm []byte) string {
	return stringField(gm, 19)
}

func parseStepUnix(meta []byte) int64 {
	tsMsg := messageField(meta, 1)
	if tsMsg == nil {
		return 0
	}
	v, ok := varintField(tsMsg, 1)
	if !ok {
		return 0
	}
	return int64(v)
}
