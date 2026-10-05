package ast

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
)

// MarshalJSON implements [json.Marshaler]. The encoding is deterministic:
// object keys follow the declared property order (or sorted order for plain
// maps), uses two-space indentation, and never escapes HTML-sensitive
// characters.
func (d *CanonicalDocument) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	cw := newCanonicalWriter(&buf)
	cw.writeByte('{')
	cw.newline(1)
	cw.writeKey("version")
	cw.writeString(": ")
	if err := cw.writeValue(d.Version, 1); err != nil {
		return nil, err
	}
	cw.writeByte(',')
	cw.newline(1)
	cw.writeKey("root")
	cw.writeString(": ")
	if err := d.Root.writeTo(cw, 1); err != nil {
		return nil, err
	}
	cw.newline(0)
	cw.writeByte('}')
	if cw.err != nil {
		return nil, cw.err
	}
	return buf.Bytes(), nil
}

type canonicalWriter struct {
	w      io.Writer
	indent []byte
	strBuf bytes.Buffer
	strEnc *json.Encoder
	err    error
}

func newCanonicalWriter(w io.Writer) *canonicalWriter {
	c := &canonicalWriter{w: w, indent: []byte("  ")}
	c.strEnc = json.NewEncoder(&c.strBuf)
	c.strEnc.SetEscapeHTML(false)
	return c
}

func (c *canonicalWriter) write(p []byte) {
	if c.err != nil {
		return
	}
	_, c.err = c.w.Write(p)
}

func (c *canonicalWriter) writeByte(b byte) {
	c.write([]byte{b})
}

func (c *canonicalWriter) writeString(s string) {
	c.write([]byte(s))
}

func (c *canonicalWriter) writeKey(k string) {
	c.write(c.jsonString(k))
}

func (c *canonicalWriter) newline(depth int) {
	if c.err != nil {
		return
	}
	c.writeByte('\n')
	for range depth {
		c.write(c.indent)
	}
}

func (n *CanonicalNode) writeTo(c *canonicalWriter, depth int) error {
	c.writeByte('{')
	c.newline(depth + 1)
	c.writeKey("type")
	c.writeString(": ")
	c.writeKey(n.Kind)

	for _, p := range n.Properties {
		c.writeByte(',')
		c.newline(depth + 1)
		c.writeKey(p.Name)
		c.writeString(": ")
		if err := c.writeValue(p.Value, depth+1); err != nil {
			return err
		}
	}

	if n.withSource {
		if n.HasStart {
			c.writeByte(',')
			c.newline(depth + 1)
			c.writeKey("start")
			c.writeString(": ")
			c.writeString(strconv.Itoa(n.Start))
		}
		if n.HasStop {
			c.writeByte(',')
			c.newline(depth + 1)
			c.writeKey("stop")
			c.writeString(": ")
			c.writeString(strconv.Itoa(n.Stop))
		}
	}

	if len(n.Attributes) > 0 {
		c.writeByte(',')
		c.newline(depth + 1)
		c.writeKey("attributes")
		c.writeString(": ")
		attrs := make([]any, len(n.Attributes))
		for i, attr := range n.Attributes {
			attrs[i] = CanonicalObject{
				{Name: "name", Value: attr.Name},
				{Name: "value", Value: attr.Value},
			}
		}
		if err := c.writeValue(attrs, depth+1); err != nil {
			return err
		}
	}

	c.writeByte(',')
	c.newline(depth + 1)
	c.writeKey("children")
	c.writeString(": ")
	if err := c.writeValue(n.Children, depth+1); err != nil {
		return err
	}
	c.newline(depth)
	c.writeByte('}')
	return c.err
}

func (c *canonicalWriter) writeValue(v any, depth int) error {
	switch v := v.(type) {
	case nil:
		c.writeString("null")
	case bool:
		c.writeString(strconv.FormatBool(v))
	case string:
		c.write(c.jsonString(v))
	case json.Number:
		c.writeString(string(v))
	case json.RawMessage:
		if !json.Valid(v) {
			return fmt.Errorf("invalid raw JSON value: %q", string(v))
		}
		c.write(v)
	case int:
		c.writeString(strconv.Itoa(v))
	case int8:
		c.writeString(strconv.FormatInt(int64(v), 10))
	case int16:
		c.writeString(strconv.FormatInt(int64(v), 10))
	case int32:
		c.writeString(strconv.FormatInt(int64(v), 10))
	case int64:
		c.writeString(strconv.FormatInt(v, 10))
	case uint:
		c.writeString(strconv.FormatUint(uint64(v), 10))
	case uint8:
		c.writeString(strconv.FormatUint(uint64(v), 10))
	case uint16:
		c.writeString(strconv.FormatUint(uint64(v), 10))
	case uint32:
		c.writeString(strconv.FormatUint(uint64(v), 10))
	case uint64:
		c.writeString(strconv.FormatUint(v, 10))
	case float32:
		if err := writeFloat(c, float64(v)); err != nil {
			return err
		}
	case float64:
		if err := writeFloat(c, v); err != nil {
			return err
		}
	case []any:
		if err := c.writeArray(len(v), func(i int) error { return c.writeValue(v[i], depth+1) }, depth); err != nil {
			return err
		}
	case CanonicalObject:
		if err := c.writeOrderedObject(v, depth); err != nil {
			return err
		}
	case []CanonicalProperty:
		if err := c.writeOrderedObject(CanonicalObject(v), depth); err != nil {
			return err
		}
	case []*CanonicalNode:
		if err := c.writeArray(len(v), func(i int) error { return v[i].writeTo(c, depth+1) }, depth); err != nil {
			return err
		}
	case map[string]any:
		if err := c.writeStringMap(v, depth); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported canonical value of type %T", v)
	}
	return c.err
}

func (c *canonicalWriter) writeArray(length int, writeElem func(i int) error, depth int) error {
	c.writeByte('[')
	for i := range length {
		if i > 0 {
			c.writeByte(',')
		}
		c.newline(depth + 1)
		if err := writeElem(i); err != nil {
			return err
		}
	}
	if length > 0 {
		c.newline(depth)
	}
	c.writeByte(']')
	return c.err
}

func (c *canonicalWriter) writeOrderedObject(obj CanonicalObject, depth int) error {
	c.writeByte('{')
	for i, p := range obj {
		if i > 0 {
			c.writeByte(',')
		}
		c.newline(depth + 1)
		c.write(c.jsonString(p.Name))
		c.writeString(": ")
		if err := c.writeValue(p.Value, depth+1); err != nil {
			return err
		}
	}
	if len(obj) > 0 {
		c.newline(depth)
	}
	c.writeByte('}')
	return c.err
}

func (c *canonicalWriter) writeStringMap(m map[string]any, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	c.writeByte('{')
	for i, k := range keys {
		if i > 0 {
			c.writeByte(',')
		}
		c.newline(depth + 1)
		c.write(c.jsonString(k))
		c.writeString(": ")
		if err := c.writeValue(m[k], depth+1); err != nil {
			return err
		}
	}
	if len(keys) > 0 {
		c.newline(depth)
	}
	c.writeByte('}')
	return c.err
}

func writeFloat(c *canonicalWriter, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("cannot encode non-finite float %v", f)
	}
	c.writeString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// jsonString returns the JSON encoding of s without HTML escaping.
// Encode appends a trailing newline that is trimmed.
func (c *canonicalWriter) jsonString(s string) []byte {
	c.strBuf.Reset()
	if err := c.strEnc.Encode(s); err != nil {
		panic(fmt.Sprintf("ast: cannot encode JSON string: %v", err))
	}
	return bytes.TrimRight(c.strBuf.Bytes(), "\n")
}
