// Copyright (c) 2026 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package zapcore

// DuplicateKeyPolicy controls how the JSON encoder reacts when the same key
// is added more than once within the same object.
//
// Policies are applied per object: the key "k" at the top level is a
// different key from "k" nested inside a namespace or an object.
type DuplicateKeyPolicy uint8

const (
	// AllowDuplicateKeys is the zero value and preserves zap's historical
	// behavior: every key/value pair is written to the output, even if that
	// produces objects with repeated keys.
	AllowDuplicateKeys DuplicateKeyPolicy = iota

	// KeepFirst keeps the first value written for a key within an object and
	// drops any subsequent values. Values that would be dropped are never
	// evaluated, so lazily-marshaled fields (ObjectMarshaler, ArrayMarshaler,
	// custom Stringers, etc.) are not invoked.
	KeepFirst

	// KeepLast keeps the last value written for a key within an object and
	// drops any earlier values. Every field is still evaluated in order, so
	// lazily-marshaled fields may observe the state of their arguments at log
	// time; the surviving value is moved to the position of the first
	// occurrence, so field order stays stable.
	KeepLast

	// ReportConflict writes every key/value pair (like AllowDuplicateKeys)
	// and invokes the configured OnDuplicateKey callback for each conflict,
	// reporting the object path where the conflict occurred and the values
	// involved.
	ReportConflict
)

// String returns the canonical name of the policy.
func (p DuplicateKeyPolicy) String() string {
	switch p {
	case KeepFirst:
		return "keepFirst"
	case KeepLast:
		return "keepLast"
	case ReportConflict:
		return "reportConflict"
	default:
		return "allowDuplicateKeys"
	}
}

// UnmarshalText unmarshals text into a DuplicateKeyPolicy. It accepts
// "keepFirst", "keepLast", and "reportConflict"; anything else (including the
// empty string) unmarshals to AllowDuplicateKeys.
func (p *DuplicateKeyPolicy) UnmarshalText(text []byte) error {
	switch string(text) {
	case "keepFirst":
		*p = KeepFirst
	case "keepLast":
		*p = KeepLast
	case "reportConflict":
		*p = ReportConflict
	default:
		*p = AllowDuplicateKeys
	}
	return nil
}

// UnmarshalYAML unmarshals YAML into a DuplicateKeyPolicy.
func (p *DuplicateKeyPolicy) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	return p.UnmarshalText([]byte(s))
}

// DuplicateKeyConflict describes a single repeated key within one object.
type DuplicateKeyConflict struct {
	// Path is the dotted path of the object that contains the repeated key,
	// e.g. "request.headers" for a conflict inside the "headers" object of the
	// "request" namespace. It is empty for the root object.
	Path string
	// Key is the repeated key within the object identified by Path.
	Key string
	// ExistingValue is the value that was already present for Key, formatted
	// as it was (or will be) written to the JSON output.
	ExistingValue string
	// NewValue is the conflicting value, formatted the same way.
	NewValue string
}

// DuplicateKeyFunc is invoked by the JSON encoder for every repeated key when
// the encoder is configured with ReportConflict. It must be safe to use for
// logging or metrics collection; implementations should avoid heavy work.
type DuplicateKeyFunc func(DuplicateKeyConflict)
