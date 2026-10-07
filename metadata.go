package rpc2

import (
	"context"
	"errors"
)

// MaxMetadataKeys bounds the number of entries allowed in the per-request
// metadata area of a MetadataCodec.
const MaxMetadataKeys = 64

// MaxMetadataLen bounds the total wire size, in bytes, of the per-request
// metadata area of a MetadataCodec.
const MaxMetadataLen = 64 * 1024

// ErrTooManyMetadataKeys is returned when an incoming request carries more
// metadata entries than MaxMetadataKeys.
var ErrTooManyMetadataKeys = errors.New("rpc2: too many metadata keys")

// ErrMetadataTooLarge is returned when an incoming request carries a metadata
// area larger than MaxMetadataLen bytes.
var ErrMetadataTooLarge = errors.New("rpc2: metadata area too large")

// Metadata is the per-request metadata carried alongside an RPC call.
// Keys are case-sensitive and map to one or more values, similar to HTTP
// headers. The maps obtained from the call context must never be mutated in
// place by handlers; modifying a returned map cannot change the view of any
// other (interleaved or nested) call.
type Metadata map[string][]string

// Get returns the first value associated with key, if any.
func (md Metadata) Get(key string) (string, bool) {
	if vs, ok := md[key]; ok && len(vs) > 0 {
		return vs[0], true
	}
	return "", false
}

// Clone returns a deep copy of md.
func (md Metadata) Clone() Metadata {
	return md.clone()
}

// clone returns a deep copy of md. The result is safe for one call's
// exclusive use and never aliases a map handed to another call.
func (md Metadata) clone() Metadata {
	if md == nil {
		return nil
	}
	out := make(Metadata, len(md))
	for k, vs := range md {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// MetadataFunc runs on the server side for every incoming request before the
// registered handler is invoked. peer contains the values submitted by the
// remote peer. The returned metadata becomes the locally confirmed view
// exposed to the handler; entries may be validated, removed or added.
// Returning an error rejects the request: the handler is never executed and
// the error is sent back as the response.
type MetadataFunc func(ctx context.Context, client *Client, method string, peer Metadata) (local Metadata, err error)

type incomingMDKey struct{}
type localMDKey struct{}
type outgoingMDKey struct{}

// NewIncomingContext returns a copy of ctx carrying peer-submitted metadata.
func NewIncomingContext(ctx context.Context, md Metadata) context.Context {
	return context.WithValue(ctx, incomingMDKey{}, md.clone())
}

// NewLocalContext returns a copy of ctx carrying locally confirmed metadata.
func NewLocalContext(ctx context.Context, md Metadata) context.Context {
	return context.WithValue(ctx, localMDKey{}, md.clone())
}

// NewOutgoingContext returns a copy of ctx carrying metadata to be sent on
// calls made with Client.CallWithContext. Passing nil clears any outgoing
// metadata inherited from a parent context.
func NewOutgoingContext(ctx context.Context, md Metadata) context.Context {
	return context.WithValue(ctx, outgoingMDKey{}, md.clone())
}

// FromIncomingContext returns the peer-submitted metadata and whether it was
// present. The returned map is a copy owned by the caller.
func FromIncomingContext(ctx context.Context) (Metadata, bool) {
	md, ok := ctx.Value(incomingMDKey{}).(Metadata)
	return md.clone(), ok
}

// FromLocalContext returns the locally confirmed metadata and whether it was
// present. The returned map is a copy owned by the caller.
func FromLocalContext(ctx context.Context) (Metadata, bool) {
	md, ok := ctx.Value(localMDKey{}).(Metadata)
	return md.clone(), ok
}

// FromOutgoingContext returns the metadata scheduled for outgoing calls and
// whether it was explicitly set (possibly to an empty map). The returned map
// is a copy owned by the caller.
func FromOutgoingContext(ctx context.Context) (Metadata, bool) {
	v := ctx.Value(outgoingMDKey{})
	if v == nil {
		return nil, false
	}
	md, _ := v.(Metadata)
	return md.clone(), true
}

// withMetadataViews stores per-call metadata directly on the context without
// copying. The maps passed in are owned exclusively by this call: peer is the
// decoded frame copy and local is the hook result or a copy of peer.
func withMetadataViews(ctx context.Context, peer, local Metadata) context.Context {
	if peer != nil {
		ctx = context.WithValue(ctx, incomingMDKey{}, peer)
	}
	if local != nil {
		ctx = context.WithValue(ctx, localMDKey{}, local)
	}
	return ctx
}

// PropagateMetadata returns a context that propagates only the given
// whitelisted keys of the peer-submitted metadata on nested (reverse) calls.
// It is the default-safe way to forward a subset of incoming metadata: values
// not in the whitelist are dropped instead of being copied automatically.
// Values present in outgoing metadata take precedence over propagated ones.
func PropagateMetadata(ctx context.Context, keys ...string) context.Context {
	incoming, _ := FromIncomingContext(ctx)
	outgoing, explicit := FromOutgoingContext(ctx)
	if outgoing == nil {
		outgoing = Metadata{}
	}
	allow := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allow[k] = struct{}{}
	}
	for k, vs := range incoming {
		if _, ok := allow[k]; !ok {
			continue
		}
		if _, already := outgoing[k]; already {
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		outgoing[k] = cp
	}
	if !explicit && len(outgoing) == 0 {
		return ctx
	}
	return context.WithValue(ctx, outgoingMDKey{}, outgoing)
}
