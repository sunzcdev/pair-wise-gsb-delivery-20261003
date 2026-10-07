package rpc2

import (
	"context"
	"errors"
)

// Limits of the per-request metadata area.
// The bounds protect a connection from a peer that sends an unbounded
// number of keys or very large values; requests that exceed them are
// rejected before any handler runs.
const (
	// MaxMetadataKeys is the maximum number of metadata keys per request.
	MaxMetadataKeys = 16
	// MaxMetadataValueLen is the maximum byte length of a single value.
	MaxMetadataValueLen = 4096
	// MaxMetadataLen is the maximum total byte length of all values.
	MaxMetadataLen = 8192
)

var (
	errMetadataTooManyKeys  = errors.New("rpc2: metadata exceeds maximum of 16 keys")
	errMetadataValueTooLong = errors.New("rpc2: metadata value exceeds maximum length of 4096 bytes")
	errMetadataTooLong      = errors.New("rpc2: metadata exceeds maximum total length of 8192 bytes")
	errMetadataEmptyKey     = errors.New("rpc2: metadata key must not be empty")
)

// Metadata carries a small, bounded set of per-request string attributes
// such as tracing identifiers or routing tenants.
type Metadata map[string]string

// MetadataCodec is an optional Codec capability. A codec that implements it
// gains a bounded metadata area in request headers. Codecs that do not
// implement it (such as the built-in gob codec) keep working with plain
// calls that carry no metadata.
type MetadataCodec interface {
	Codec

	// ReadRequestMetadata returns the metadata submitted by the peer for
	// the request whose header was most recently read with ReadHeader.
	// It is called before the request body is decoded and before any
	// handler runs, so malformed or oversized metadata can be rejected
	// up front.
	ReadRequestMetadata() (Metadata, error)
}

// MetadataInterceptor runs on the receiving side after the request header
// and metadata are decoded but before the request body is read and before
// the handler is invoked.
//
// peer contains the values submitted by the remote side. It is a private
// copy owned by this call; the interceptor may inspect or delete entries
// without affecting any other request or handler. confirmed is the local
// view of the values that the local side acknowledges for the handler;
// the interceptor may populate or alter it freely.
//
// A non-nil error rejects the request: no handler runs and the error is
// returned to the caller as an RPC error.
type MetadataInterceptor func(ctx context.Context, peer, confirmed Metadata) error

type contextKey int

const (
	peerMetadataKey contextKey = iota
	confirmedMetadataKey
	outgoingMetadataKey
	metadataWhitelistKey
)

// PeerMetadata returns the metadata values submitted by the remote peer
// for the in-flight call carried by ctx. It never returns nil. The map is
// a private copy: mutating it changes neither the values seen by any other
// handler nor the wire message.
func PeerMetadata(ctx context.Context) Metadata {
	if ctx == nil {
		return Metadata{}
	}
	if md, ok := ctx.Value(peerMetadataKey).(Metadata); ok {
		return cloneMetadata(md)
	}
	return Metadata{}
}

// ConfirmedMetadata returns the locally acknowledged metadata for the
// in-flight call carried by ctx. It is populated by the receiving side's
// MetadataInterceptor (validation, deletion or local augmentation); if no
// interceptor is installed it mirrors the peer-submitted values. It never
// returns nil. The map is a private copy.
func ConfirmedMetadata(ctx context.Context) Metadata {
	if ctx == nil {
		return Metadata{}
	}
	if md, ok := ctx.Value(confirmedMetadataKey).(Metadata); ok {
		return cloneMetadata(md)
	}
	return Metadata{}
}

// WithMetadata attaches attributes to an outgoing call. It is used with
// CallContext or NotifyContext. When used inside a handler for a nested
// reverse RPC, these values are merged on top of the whitelisted inbound
// attributes (see WithMetadataWhitelist).
func WithMetadata(ctx context.Context, md Metadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := cloneMetadata(outgoingMetadata(ctx))
	for k, v := range md {
		merged[k] = v
	}
	return context.WithValue(ctx, outgoingMetadataKey, merged)
}

// WithMetadataWhitelist restricts the inbound metadata propagated by a
// nested reverse RPC made with ctx to the named keys. By default no
// inbound attribute is propagated automatically; only keys listed here are
// carried over, preferring the locally confirmed value and falling back to
// the peer-submitted value.
func WithMetadataWhitelist(ctx context.Context, keys ...string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	wl := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		wl[k] = struct{}{}
	}
	return context.WithValue(ctx, metadataWhitelistKey, wl)
}

func outgoingMetadata(ctx context.Context) Metadata {
	if ctx == nil {
		return nil
	}
	if md, ok := ctx.Value(outgoingMetadataKey).(Metadata); ok {
		return md
	}
	return nil
}

func metadataWhitelist(ctx context.Context) map[string]struct{} {
	if ctx == nil {
		return nil
	}
	if wl, ok := ctx.Value(metadataWhitelistKey).(map[string]struct{}); ok {
		return wl
	}
	return nil
}

// inboundCall reports whether ctx belongs to a request received from a
// peer, i.e. the call may be a nested reverse RPC.
func inboundCall(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, peerOK := ctx.Value(peerMetadataKey).(Metadata)
	_, confirmedOK := ctx.Value(confirmedMetadataKey).(Metadata)
	return peerOK || confirmedOK
}

func cloneMetadata(md Metadata) Metadata {
	out := make(Metadata, len(md))
	for k, v := range md {
		out[k] = v
	}
	return out
}

// checkMetadata enforces the bounded metadata limits.
func checkMetadata(md Metadata) error {
	if len(md) > MaxMetadataKeys {
		return errMetadataTooManyKeys
	}
	total := 0
	for k, v := range md {
		if k == "" {
			return errMetadataEmptyKey
		}
		if len(v) > MaxMetadataValueLen {
			return errMetadataValueTooLong
		}
		total += len(v)
	}
	if total > MaxMetadataLen {
		return errMetadataTooLong
	}
	return nil
}

// buildOutboundMetadata computes the metadata to attach to a request.
//
// Explicit attributes supplied through WithMetadata are always included.
// When the call is a nested reverse RPC (ctx comes from an inbound
// request), inbound attributes are copied only when their key appears in
// the effective whitelist (per-call WithMetadataWhitelist, else the
// connection default). Confirmed (locally acknowledged) values take
// precedence over raw peer values. Top-level calls made without an inbound
// context never auto-propagate anything.
func (c *Client) buildOutboundMetadata(ctx context.Context) Metadata {
	explicit := outgoingMetadata(ctx)

	var propagated Metadata
	if inboundCall(ctx) {
		wl := metadataWhitelist(ctx)
		if wl == nil {
			wl = c.metadataWhitelist
		}
		if len(wl) > 0 {
			propagated = make(Metadata)
			peer := PeerMetadata(ctx)
			confirmed := ConfirmedMetadata(ctx)
			for k := range wl {
				if v, ok := confirmed[k]; ok {
					propagated[k] = v
				} else if v, ok := peer[k]; ok {
					propagated[k] = v
				}
			}
		}
	}

	if len(explicit) == 0 && len(propagated) == 0 {
		return nil
	}
	out := make(Metadata, len(propagated)+len(explicit))
	for k, v := range propagated {
		out[k] = v
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}
