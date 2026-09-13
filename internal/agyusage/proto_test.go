package agyusage

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendKey(b []byte, num, wt int) []byte {
	return appendVarint(b, uint64(num<<3|wt))
}

func appendVarintField(b []byte, num int, v uint64) []byte {
	b = appendKey(b, num, wireVarint)
	return appendVarint(b, v)
}

func appendBytesField(b []byte, num int, payload []byte) []byte {
	b = appendKey(b, num, wireBytes)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func appendStringField(b []byte, num int, s string) []byte {
	return appendBytesField(b, num, []byte(s))
}

func encodeUsageBag(prompt, cached, completion, reasoning uint64) []byte {
	var b []byte
	if prompt != 0 {
		b = appendVarintField(b, 1, prompt)
	}
	if cached != 0 {
		b = appendVarintField(b, 2, cached)
	}
	if completion != 0 {
		b = appendVarintField(b, 3, completion)
	}
	if reasoning != 0 {
		b = appendVarintField(b, 5, reasoning)
	}
	return b
}

func encodeKV(key, value string) []byte {
	var b []byte
	b = appendStringField(b, 1, key)
	b = appendStringField(b, 2, value)
	return b
}

func encodeStepMeta(unix int64) []byte {
	var inner []byte
	inner = appendVarintField(inner, 1, uint64(unix))
	return appendBytesField(nil, 1, inner)
}

func encodeGenMeta(model, requestID string, usage []byte) []byte {
	var gm []byte
	gm = appendBytesField(gm, 4, usage)
	if model != "" {
		gm = appendStringField(gm, 19, model)
	}
	if requestID != "" {
		gm = appendBytesField(gm, 20, encodeKV("request_id", requestID))
	}
	return appendBytesField(nil, 1, gm)
}
