package message

import (
	"mime"

	"github.com/emersion/go-message/textproto"
)

func parseHeaderWithParams(s string) (f string, params map[string]string, err error) {
	f, params, err = mime.ParseMediaType(s)
	if err != nil {
		return s, nil, err
	}
	for k, v := range params {
		params[k], _ = decodeHeader(v)
	}
	return
}

// HeaderFields iterates over header fields.
type HeaderFields interface {
	textproto.HeaderFields

	// Text parses the value of the current field as plaintext. The field
	// charset is decoded to UTF-8. If the header field's charset is unknown,
	// the raw field value is returned and the error verifies IsUnknownCharset.
	Text() (string, error)

	headerFields()
}

type headerFields struct {
	textproto.HeaderFields
}

func (*headerFields) headerFields() {}

func (hf *headerFields) Text() (string, error) {
	return decodeHeader(hf.Value())
}

// A Header represents the key-value pairs in a message header.
type Header struct {
	textproto.Header

	// ParamLineBudget is the maximum length, in bytes, of a header field
	// line produced when formatting Content-Type and Content-Disposition
	// parameters with SetContentType and SetContentDisposition. Non-ASCII
	// parameter values that don't fit are split into RFC 2231 continuation
	// segments. If zero, DefaultParamLineBudget is used. If negative,
	// RFC 2231 parameter encoding is disabled.
	ParamLineBudget int
}

// paramLineBudget returns the configured parameter line budget, or the
// default if none is set.
func (h *Header) paramLineBudget() int {
	if h.ParamLineBudget == 0 {
		return DefaultParamLineBudget
	}
	return h.ParamLineBudget
}

// setParamsField formats and sets a Content-Type or Content-Disposition
// header field. If the formatted field doesn't fit in the parameter line
// budget, it is folded into a raw header field with every line within the
// budget.
func (h *Header) setParamsField(key, f string, params map[string]string) {
	budget := h.paramLineBudget()
	value := formatParams(f, params, budget)

	// textproto folds header fields at 76 bytes, so a field that fits both
	// the budget and that limit can go through the regular path
	limit := budget
	if limit > DefaultParamLineBudget {
		limit = DefaultParamLineBudget
	}
	if budget < 0 || len(key)+2+len(value) <= limit {
		h.Set(key, value)
		return
	}

	h.Del(key)
	h.AddRaw(foldParamsField(key, value, budget))
}

// reformatParams re-formats the Content-Type and Content-Disposition header
// fields with the header's ParamLineBudget. Fields whose parameters don't
// need encoding are left untouched.
func (h *Header) reformatParams() {
	if disp, params, err := h.ContentDisposition(); err == nil && paramsNeedEncoding(params) {
		h.SetContentDisposition(disp, params)
	}
	if t, params, err := h.ContentType(); err == nil && paramsNeedEncoding(params) {
		h.SetContentType(t, params)
	}
}

func paramsNeedEncoding(params map[string]string) bool {
	for _, v := range params {
		if paramNeedsEncoding(v) {
			return true
		}
	}
	return false
}

// HeaderFromMap creates a header from a map of header fields.
//
// This function is provided for interoperability with the standard library.
// If possible, ReadHeader should be used instead to avoid loosing information.
// The map representation looses the ordering of the fields, the capitalization
// of the header keys, and the whitespace of the original header.
func HeaderFromMap(m map[string][]string) Header {
	return Header{Header: textproto.HeaderFromMap(m)}
}

// ContentType parses the Content-Type header field.
//
// If no Content-Type is specified, it returns "text/plain".
func (h *Header) ContentType() (t string, params map[string]string, err error) {
	v := h.Get("Content-Type")
	if v == "" {
		return "text/plain", nil, nil
	}
	return parseHeaderWithParams(v)
}

// SetContentType formats the Content-Type header field.
func (h *Header) SetContentType(t string, params map[string]string) {
	h.setParamsField("Content-Type", t, params)
}

// ContentDisposition parses the Content-Disposition header field, as defined in
// RFC 2183.
func (h *Header) ContentDisposition() (disp string, params map[string]string, err error) {
	return parseHeaderWithParams(h.Get("Content-Disposition"))
}

// SetContentDisposition formats the Content-Disposition header field, as
// defined in RFC 2183.
func (h *Header) SetContentDisposition(disp string, params map[string]string) {
	h.setParamsField("Content-Disposition", disp, params)
}

// Text parses a plaintext header field. The field charset is automatically
// decoded to UTF-8. If the header field's charset is unknown, the raw field
// value is returned and the error verifies IsUnknownCharset.
func (h *Header) Text(k string) (string, error) {
	return decodeHeader(h.Get(k))
}

// SetText sets a plaintext header field.
func (h *Header) SetText(k, v string) {
	h.Set(k, encodeHeader(v))
}

// Copy creates a stand-alone copy of the header.
func (h *Header) Copy() Header {
	return Header{Header: h.Header.Copy(), ParamLineBudget: h.ParamLineBudget}
}

// Fields iterates over all the header fields.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) Fields() HeaderFields {
	return &headerFields{h.Header.Fields()}
}

// FieldsByKey iterates over all fields having the specified key.
//
// The header may not be mutated while iterating, except using HeaderFields.Del.
func (h *Header) FieldsByKey(k string) HeaderFields {
	return &headerFields{h.Header.FieldsByKey(k)}
}
