// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnknownFieldNotExported(t *testing.T) {
	reg := testRegistry()
	// Resource/Key allowed; Token must never be emitted.
	in := &notFound{Resource: "r", Key: "k", Token: "secret"}
	env, ok := encodeRoot(reg, in)
	if !ok {
		t.Fatal("expected an envelope")
	}
	raw, _ := json.Marshal(env)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "Token") {
		t.Fatalf("sensitive field leaked into envelope: %s", raw)
	}
}

func TestHookCannotBypassAllowList(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(Registration{
		TypeID:         "t",
		ExportedFields: []string{"ok"},
		Encode: func(error) (map[string]any, error) {
			return map[string]any{"ok": 1, "evil": "leak"}, nil
		},
	})

	type unregistered struct{}
	// A standalone error type driven through the prototype-less fallback
	// hook, which tries to emit a field ("evil") not on the allow-list.
	env, ok := encodeRoot(reg, hookError{})
	if !ok {
		t.Fatal("envelope expected")
	}
	raw, _ := json.Marshal(env)
	if strings.Contains(string(raw), "leak") || strings.Contains(string(raw), "evil") {
		t.Fatalf("hook bypassed the allow-list: %s", raw)
	}
	if _, exists := env.Nodes[0].Fields["ok"]; !exists {
		t.Fatal("allowed field missing")
	}
}

// hookError is only matched by the prototype-less Encode hook registered in
// TestHookCannotBypassAllowList.
type hookError struct{}

func (hookError) Error() string { return "hook" }

func TestTooManyCausesTruncated(t *testing.T) {
	reg := testRegistry()

	var causes []error
	for i := 0; i < MaxCauses+5; i++ {
		causes = append(causes, &notFound{Resource: "r"})
	}
	root := &multi{msg: "many", causes: causes}

	env, ok := encodeRoot(reg, root)
	if !ok {
		t.Fatal("envelope expected")
	}

	// Root keeps at most MaxCauses cause slots (the last may be a limit
	// placeholder).
	if len(env.Nodes[0].Causes) > MaxCauses {
		t.Fatalf("causes not bounded: %d", len(env.Nodes[0].Causes))
	}

	rebuilt, ok := decodeRoot(reg, env)
	if !ok {
		t.Fatal("decode failed")
	}
	if !errors.Is(rebuilt, ErrLimitExceeded) {
		t.Fatal("expected ErrLimitExceeded marker in truncated tree")
	}
}

func TestMalformedEnvelopeFallsBackToPlain(t *testing.T) {
	reg := testRegistry()

	// Bad version / empty nodes are rejected and surfaced verbatim.
	for _, payload := range []string{
		netRPCMarker + `{"version":99,"root":1,"nodes":[]}`,
		netRPCMarker + "not-json",
		netRPCMarker + `{"version":1,"root":99,"nodes":[]}`,
	} {
		out := reg.NetRPCDecode(errors.New(payload))
		var be *Error
		if errors.As(out, &be) {
			t.Fatalf("malformed envelope must not decode to business error: %v", out)
		}
	}
}

func TestDiamondSharedNode(t *testing.T) {
	reg := testRegistry()

	shared := &notFound{Resource: "same"}
	root := &multi{msg: "root", causes: []error{shared, shared}}

	env, ok := encodeRoot(reg, root)
	if !ok {
		t.Fatal("envelope expected")
	}

	// Two causes pointing at the same node should be encoded once plus an
	// alias, not duplicated.
	if len(env.Nodes) != 3 {
		t.Fatalf("expected root + shared node + alias, got %d nodes", len(env.Nodes))
	}
	if env.Nodes[2].Ref != 2 {
		t.Fatalf("third node should alias the shared node, got ref=%d", env.Nodes[2].Ref)
	}

	rebuilt, ok := decodeRoot(reg, env)
	if !ok {
		t.Fatal("decode failed")
	}
	var nf *notFound
	if !errors.As(rebuilt, &nf) {
		t.Fatal("shared node type not reachable")
	}
}

func TestGenericErrorByTypeID(t *testing.T) {
	reg := testRegistry()
	in := &Error{TypeID: "test.not_found", Message: "via generic", Fields: map[string]any{"Resource": "g"}}

	env, ok := encodeRoot(reg, in)
	if !ok {
		t.Fatal("envelope expected")
	}
	rebuilt, ok := decodeRoot(reg, env)
	if !ok {
		t.Fatal("decode failed")
	}

	// Even though it was emitted generically, the known type id reconstructs
	// the registered concrete type.
	var nf *notFound
	if !errors.As(rebuilt, &nf) || nf.Resource != "g" {
		t.Fatalf("registered type not reconstructed from generic emission: %#v", rebuilt)
	}
}
