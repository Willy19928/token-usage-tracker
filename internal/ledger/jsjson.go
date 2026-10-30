package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// jsValue is a JSON value that keeps object key order, so re-encoding it
// reproduces JSON.stringify(JSON.parse(text)). Antigravity text estimates are
// character counts of that re-encoding, so the re-encoding must be exact.
type jsValue struct {
	kind   byte // 'n' null, 'b' bool, 'f' number, 's' string, 'a' array, 'o' object
	b      bool
	f      float64
	s      string
	items  []jsValue
	keys   []string
	values map[string]*jsValue
}

func parseJS(data []byte) (jsValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeJS(decoder)
	if err != nil {
		return jsValue{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return jsValue{}, fmt.Errorf("unexpected trailing JSON")
	}
	return value, nil
}

func decodeJS(decoder *json.Decoder) (jsValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return jsValue{}, err
	}
	switch value := token.(type) {
	case nil:
		return jsValue{kind: 'n'}, nil
	case bool:
		return jsValue{kind: 'b', b: value}, nil
	case json.Number:
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil && !strings.Contains(err.Error(), "range") {
			return jsValue{}, err
		}
		return jsValue{kind: 'f', f: number}, nil
	case string:
		return jsValue{kind: 's', s: value}, nil
	case json.Delim:
		if value == '[' {
			out := jsValue{kind: 'a'}
			for decoder.More() {
				item, err := decodeJS(decoder)
				if err != nil {
					return jsValue{}, err
				}
				out.items = append(out.items, item)
			}
			_, err := decoder.Token()
			return out, err
		}
		out := jsValue{kind: 'o', values: map[string]*jsValue{}}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return jsValue{}, err
			}
			key := keyToken.(string)
			item, err := decodeJS(decoder)
			if err != nil {
				return jsValue{}, err
			}
			// Like JSON.parse, a repeated key keeps its first position and last value.
			if existing, ok := out.values[key]; ok {
				*existing = item
				continue
			}
			out.keys = append(out.keys, key)
			stored := item
			out.values[key] = &stored
		}
		_, err := decoder.Token()
		return out, err
	}
	return jsValue{}, fmt.Errorf("unsupported JSON token")
}

func (v jsValue) get(key string) (jsValue, bool) {
	if v.kind != 'o' {
		return jsValue{}, false
	}
	item, ok := v.values[key]
	if !ok {
		return jsValue{}, false
	}
	return *item, true
}

func (v jsValue) str(key string) (string, bool) {
	item, ok := v.get(key)
	if !ok || item.kind != 's' {
		return "", false
	}
	return item.s, true
}

// safeInt mirrors Number.isSafeInteger.
func (v jsValue) safeInt(key string) (int64, bool) {
	item, ok := v.get(key)
	if !ok || item.kind != 'f' {
		return 0, false
	}
	return safeIntFloat(item.f)
}

func safeIntFloat(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || math.Abs(value) > 9007199254740991 {
		return 0, false
	}
	return int64(value), true
}

// truthy mirrors JavaScript truthiness. The zero jsValue (a missing field, as
// returned by get) is falsy like undefined.
func (v jsValue) truthy() bool {
	switch v.kind {
	case 0, 'n':
		return false
	case 'b':
		return v.b
	case 'f':
		return v.f != 0 && !math.IsNaN(v.f)
	case 's':
		return v.s != ""
	}
	return true
}

func (v jsValue) stringify() string {
	var builder strings.Builder
	v.write(&builder)
	return builder.String()
}

func (v jsValue) write(builder *strings.Builder) {
	switch v.kind {
	case 'n':
		builder.WriteString("null")
	case 'b':
		builder.WriteString(strconv.FormatBool(v.b))
	case 'f':
		builder.WriteString(jsNumber(v.f))
	case 's':
		writeJSString(builder, v.s)
	case 'a':
		builder.WriteByte('[')
		for index, item := range v.items {
			if index > 0 {
				builder.WriteByte(',')
			}
			item.write(builder)
		}
		builder.WriteByte(']')
	case 'o':
		builder.WriteByte('{')
		for index, key := range v.keys {
			if index > 0 {
				builder.WriteByte(',')
			}
			writeJSString(builder, key)
			builder.WriteByte(':')
			v.values[key].write(builder)
		}
		builder.WriteByte('}')
	}
}

func writeJSString(builder *strings.Builder, value string) {
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(builder, `\u%04x`, r)
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte('"')
}

// jsNumber mirrors Number.prototype.toString for finite values; JSON.stringify
// writes non-finite numbers as null.
func jsNumber(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	mantissa := strconv.FormatFloat(value, 'e', -1, 64) // d.ddde±xx
	parts := strings.SplitN(mantissa, "e", 2)
	digits := strings.Replace(parts[0], ".", "", 1)
	exponent, _ := strconv.Atoi(parts[1])
	n := exponent + 1 // position of the decimal point relative to digits
	k := len(digits)
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e := n - 1
	expSign := "+"
	if e < 0 {
		expSign, e = "-", -e
	}
	if k == 1 {
		return sign + digits + "e" + expSign + strconv.Itoa(e)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + expSign + strconv.Itoa(e)
}

// jsTrim mirrors String.prototype.trim, which also strips a byte-order mark.
func jsTrim(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
			return true
		}
		return r >= 0x2000 && r <= 0x200A
	})
}

func runeCount(value string) int { return utf8.RuneCountInString(value) }
