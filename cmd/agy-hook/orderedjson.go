package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// orderedPair and orderedMap preserve JSON object key order the way a
// Python dict (json.loads with the default object_pairs_hook) does. This
// matters because the Python script falls back to `json.dumps(args)` for
// an unrecognized tool_call.args shape, and that output's key order and
// spacing must match byte-for-byte for the golden tests to hold.
type orderedPair struct {
	Key   string
	Value any
}

type orderedMap []orderedPair

// MarshalJSON lets an orderedMap round-trip through encoding/json when it
// ends up nested inside an envelope value (e.g. a structured
// terminationReason passed through verbatim) — standard key/value JSON,
// order preserved, not the Python-spacing form pyDumps produces.
func (m orderedMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(p.Key)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(p.Value)
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Get returns the value for key and whether it was present — same shape
// as Python's dict.get returning (value, ok) via a membership check.
func (m orderedMap) Get(key string) (any, bool) {
	for _, p := range m {
		if p.Key == key {
			return p.Value, true
		}
	}
	return nil, false
}

// GetString returns the string value for key, or "" if absent or not a
// string — mirrors the common `payload.get(key, "")` pattern used
// throughout the Python script for plain text fields.
func (m orderedMap) GetString(key string) string {
	v, ok := m.Get(key)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// decodePayload parses stdin JSON the way the Python script's
// `json.load(sys.stdin)` does, falling back to an empty object on any
// decode error (`except Exception: payload = {}`). Numbers decode as
// json.Number so their original literal text can be reproduced exactly
// (needed for the json.dumps(args) fallback path).
func decodePayload(data []byte) orderedMap {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeAny(dec)
	if err != nil {
		return orderedMap{}
	}
	if om, ok := v.(orderedMap); ok {
		return om
	}
	return orderedMap{}
}

// decodeAny recursively decodes the next JSON value from dec, using
// orderedMap for objects (to preserve key order) and []any for arrays.
func decodeAny(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			om := orderedMap{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				val, err := decodeAny(dec)
				if err != nil {
					return nil, err
				}
				om = append(om, orderedPair{Key: key, Value: val})
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return om, nil
		case '[':
			var arr []any
			for dec.More() {
				val, err := decodeAny(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return tok, nil // string, bool, nil, json.Number
	}
}

// pythonTruthy mirrors Python's truthiness for JSON-decoded values: only
// "", 0, false, null, and empty list/dict are falsy — everything else,
// including whitespace-only strings and negative numbers, is truthy.
func pythonTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return true
		}
		return f != 0
	case string:
		return x != ""
	case []any:
		return len(x) != 0
	case orderedMap:
		return len(x) != 0
	default:
		return true
	}
}

// pyStr approximates Python's str() for the JSON-decoded types that
// realistically appear in an agy `error` field. Strings and numbers are
// exact; composite fallbacks (list/dict) are best-effort and not a
// verified match against CPython's repr — no observed agy payload has
// used a non-string error value.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// pyDumps renders v the way Python's json.dumps(v) does with default
// arguments: ", " item separator, ": " key separator, and ensure_ascii=
// true (non-ASCII runes escaped as \uXXXX). Only used for the
// tool_call.args fallback where no known summary field is present.
func pyDumps(v any) string {
	var b strings.Builder
	writePyJSON(&b, v)
	return b.String()
}

func writePyJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(string(x))
	case string:
		writePyString(b, x)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, item)
		}
		b.WriteByte(']')
	case orderedMap:
		b.WriteByte('{')
		for i, pair := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, pair.Key)
			b.WriteString(": ")
			writePyJSON(b, pair.Value)
		}
		b.WriteByte('}')
	default:
		// Should not happen for values produced by decodeAny.
		b.WriteString(strconv.Quote(fmt.Sprintf("%v", x)))
	}
}

// writePyString escapes s the way Python's json.dumps does by default:
// ensure_ascii=True turns every non-ASCII rune into a \uXXXX escape
// (surrogate pairs for astral code points).
func writePyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case r < 0x7f:
				b.WriteRune(r)
			case r <= 0xffff:
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
			}
		}
	}
	b.WriteByte('"')
}

// truncateRunes matches Python's `s[:max]`, which slices by code point,
// not byte.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
