package dsml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

func encodeJSONString(p []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(p) + 2)
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.Bytes()
}

func decodeJSONString(raw json.RawMessage) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	in := raw[1 : len(raw)-1]
	var out []byte
	for i := 0; i < len(in); i++ {
		if in[i] != '\\' {
			out = append(out, in[i])
			continue
		}
		if i+1 >= len(in) {
			return nil, false
		}
		i++
		switch in[i] {
		case '"', '\\', '/':
			out = append(out, in[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			if i+4 >= len(in) {
				return nil, false
			}
			r, ok := parseHexRune(in[i+1 : i+5])
			if !ok {
				return nil, false
			}
			i += 4
			var tmp [utf8.UTFMax]byte
			n := utf8.EncodeRune(tmp[:], r)
			out = append(out, tmp[:n]...)
		default:
			return nil, false
		}
	}
	return out, true
}

func parseHexRune(h []byte) (rune, bool) {
	if len(h) != 4 {
		return 0, false
	}
	var v rune
	for _, c := range h {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return v, true
}
