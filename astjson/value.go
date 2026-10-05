package astjson

import (
	"bytes"
	"encoding/json"
)

// orderedField is one entry of an [orderedObject].
type orderedField struct {
	key   string
	value jsonValue
	// omitEmpty omits this field when its value is an empty array, an empty
	// object, the empty string or nil.
	omitEmpty bool
}

// orderedObject is a JSON object whose fields are emitted in insertion order.
// It must never be constructed by iterating over a Go map.
type orderedObject struct {
	fields []orderedField
}

func newObject(fields ...orderedField) *orderedObject {
	return &orderedObject{fields: fields}
}

func (o *orderedObject) visibleFields() []orderedField {
	out := make([]orderedField, 0, len(o.fields))
	for _, f := range o.fields {
		if f.omitEmpty && isEmptyValue(f.value) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (o *orderedObject) isEmpty() bool {
	return len(o.visibleFields()) == 0
}

func (o *orderedObject) writeIndented(buf *bytes.Buffer, indent string, level int) error {
	fields := o.visibleFields()
	if len(fields) == 0 {
		buf.WriteString("{}")
		return nil
	}
	buf.WriteString("{\n")
	for i, f := range fields {
		writeIndent(buf, indent, level+1)
		key, err := marshalPrimitive(f.key)
		if err != nil {
			return err
		}
		buf.Write(key)
		buf.WriteString(": ")
		if err := writeIndentedValue(buf, f.value, indent, level+1); err != nil {
			return err
		}
		if i < len(fields)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	writeIndent(buf, indent, level)
	buf.WriteByte('}')
	return nil
}

// jsonArray is a JSON array emitted in slice order.
type jsonArray []jsonValue

func (a jsonArray) writeIndented(buf *bytes.Buffer, indent string, level int) error {
	if len(a) == 0 {
		buf.WriteString("[]")
		return nil
	}
	buf.WriteString("[\n")
	for i, v := range a {
		writeIndent(buf, indent, level+1)
		if err := writeIndentedValue(buf, v, indent, level+1); err != nil {
			return err
		}
		if i < len(a)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	writeIndent(buf, indent, level)
	buf.WriteByte(']')
	return nil
}

// jsonValue is any value that can appear in the canonical output.
type jsonValue any

// writeCompactValue serializes v as compact JSON with ordered keys.
func writeCompactValue(buf *bytes.Buffer, v jsonValue) error {
	switch v := v.(type) {
	case *orderedObject:
		buf.WriteByte('{')
		for i, f := range v.visibleFields() {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, err := marshalPrimitive(f.key)
			if err != nil {
				return err
			}
			buf.Write(key)
			buf.WriteByte(':')
			if err := writeCompactValue(buf, f.value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case jsonArray:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCompactValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		b, err := marshalPrimitive(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

func writeIndentedValue(buf *bytes.Buffer, v jsonValue, indent string, level int) error {
	switch v := v.(type) {
	case *orderedObject:
		return v.writeIndented(buf, indent, level)
	case jsonArray:
		return v.writeIndented(buf, indent, level)
	default:
		b, err := marshalPrimitive(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

// marshalPrimitive serializes primitive JSON values without escaping HTML
// characters such as '&' and '<', keeping materialized text byte-stable and
// readable.
func marshalPrimitive(v jsonValue) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeIndent(buf *bytes.Buffer, indent string, level int) {
	for i := 0; i < level; i++ {
		buf.WriteString(indent)
	}
}

func isEmptyValue(v jsonValue) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case jsonArray:
		return len(v) == 0
	case *orderedObject:
		return v == nil || v.isEmpty()
	default:
		return false
	}
}
