package rpc2

import (
	"context"
	"testing"
)

func TestMetadataContextCopiesAreIsolated(t *testing.T) {
	src := Metadata{"k": {"v1", "v2"}}
	ctx := NewIncomingContext(context.Background(), src)
	src["k"][0] = "mutated-source"

	got, ok := FromIncomingContext(ctx)
	if !ok {
		t.Fatal("missing incoming metadata")
	}
	if v, _ := got.Get("k"); v != "v1" {
		t.Fatalf("NewIncomingContext must copy values, got %q", v)
	}

	got["k"][0] = "mutated-view"
	again, _ := FromIncomingContext(ctx)
	if v, _ := again.Get("k"); v != "v1" {
		t.Fatalf("FromIncomingContext must return a fresh copy, got %q", v)
	}
}

func TestPropagateMetadataWhitelistOnly(t *testing.T) {
	peer := Metadata{
		"tenant":  {"acme"},
		"traceid": {"abc"},
		"secret":  {"do-not-leak"},
	}
	ctx := NewIncomingContext(context.Background(), peer)
	ctx = PropagateMetadata(ctx, "tenant", "traceid")

	out, ok := FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("expected outgoing metadata after whitelist propagation")
	}
	if len(out) != 2 {
		t.Fatalf("expected two whitelisted keys, got %v", keys(out))
	}
	if _, ok := out["secret"]; ok {
		t.Fatal("non-whitelisted key must not propagate")
	}

	// Without a whitelist match, nothing is scheduled.
	ctx2 := NewIncomingContext(context.Background(), peer)
	ctx2 = PropagateMetadata(ctx2, "missing")
	if _, ok := FromOutgoingContext(ctx2); ok {
		t.Fatal("empty propagation must not set outgoing metadata")
	}

	// Explicit outgoing values win over propagated ones.
	ctx3 := NewIncomingContext(context.Background(), peer)
	ctx3 = NewOutgoingContext(ctx3, Metadata{"tenant": {"override"}})
	ctx3 = PropagateMetadata(ctx3, "tenant", "traceid")
	out3, _ := FromOutgoingContext(ctx3)
	if v, _ := out3.Get("tenant"); v != "override" {
		t.Fatalf("explicit outgoing value must win, got %q", v)
	}
	if _, ok := out3["traceid"]; !ok {
		t.Fatal("other whitelisted keys should still be propagated")
	}
}
