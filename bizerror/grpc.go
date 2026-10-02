// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// The JSON keys below are the envelope marker inside the Struct; they live
// in a dedicated namespace so ordinary Struct-returning services never
// collide with the protocol.
const (
	structKeyEnvelope = "__go_plugin_bizerror_v1__"
)

// GRPCServerInterceptor returns a unary server interceptor that converts
// registered business errors returned by plugin handlers into a gRPC status
// carrying the structured envelope. Errors that are not business errors are
// passed through untouched (and an existing *status.Status is always
// preserved), so transport-level failures keep their normal code.
//
// Register it on the plugin side:
//
//	grpc.NewServer(grpc.UnaryInterceptor(bizerror.GRPCServerInterceptor()))
func GRPCServerInterceptor(opts ...GRPCOption) grpc.UnaryServerInterceptor {
	return defaultRegistry.GRPCServerInterceptor(opts...)
}

// GRPCClientInterceptor returns a unary client interceptor that recognises
// the envelope detail and rebuilds the original error graph through the
// registry. status errors without the detail (including codes.Unavailable
// from a dead connection) are returned unchanged.
//
// Register it on the host side dial:
//
//	grpc.Dial(addr, grpc.WithUnaryInterceptor(bizerror.GRPCClientInterceptor()))
func GRPCClientInterceptor(opts ...GRPCOption) grpc.UnaryClientInterceptor {
	return defaultRegistry.GRPCClientInterceptor(opts...)
}

// GRPCOption configures the interceptors.
type GRPCOption func(*grpcConfig)

type grpcConfig struct {
	code codes.Code
}

// WithGRPCCode sets the gRPC status code used when a business error did not
// already carry a status. Defaults to codes.Unknown, the same code gRPC
// assigns to a plain error returned from a handler.
func WithGRPCCode(code codes.Code) GRPCOption {
	return func(c *grpcConfig) { c.code = code }
}

func (r *Registry) GRPCServerInterceptor(opts ...GRPCOption) grpc.UnaryServerInterceptor {
	cfg := grpcConfig{code: codes.Unknown}
	for _, opt := range opts {
		opt(&cfg)
	}

	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}

		// Encode first: status.Convert below calls errors.As, which walks
		// Unwrap forever on a cyclic error. The encoder handles cycles
		// safely; once a business error is found we never touch the
		// original error graph again.
		env, ok := encodeRoot(r, err)
		if !ok {
			// Plain error or pure transport/status error: leave it alone.
			return resp, status.Convert(err).Err()
		}

		detail, detailErr := envelopeToStruct(env)
		if detailErr != nil {
			return resp, status.New(cfg.code, safeMessage(err)).Err()
		}

		// Preserve an explicit gRPC status code when the business error is
		// also a status error. A direct interface assertion (not errors.As)
		// is used on purpose: it never walks Unwrap and is therefore safe
		// even when the error graph is cyclic.
		code := cfg.code
		if gs, ok := err.(interface{ GRPCStatus() *status.Status }); ok {
			if existing := gs.GRPCStatus(); existing != nil && existing.Code() != codes.OK {
				code = existing.Code()
			}
		}

		// The message is taken from the encoded root (already cycle-free);
		// err.Error() itself would also be safe here but the envelope is the
		// source of truth.
		out, attachErr := status.New(code, env.Nodes[env.Root-1].Message).WithDetails(detail)
		if attachErr != nil {
			return resp, status.New(cfg.code, safeMessage(err)).Err()
		}
		return resp, out.Err()
	}
}

func (r *Registry) GRPCClientInterceptor(opts ...GRPCOption) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err == nil {
			return nil
		}
		return r.GRPCDecode(err)
	}
}

// GRPCDecode rebuilds a business error from a gRPC status detail. It is
// exported for code paths that cannot use the interceptor (manual
// status.Details inspection, stream RPCs). Errors without an envelope are
// returned unchanged.
func GRPCDecode(err error) error {
	return defaultRegistry.GRPCDecode(err)
}

func (r *Registry) GRPCDecode(err error) error {
	if err == nil {
		return nil
	}
	st := status.Convert(err)
	for _, detail := range st.Details() {
		s, ok := detail.(*structpb.Struct)
		if !ok {
			continue
		}
		if env, ok := envelopeFromStruct(s); ok {
			if rebuilt, ok := decodeRoot(r, env); ok {
				return rebuilt
			}
		}
	}

	// Fallback: an envelope sent as message text (e.g. a net/rpc plugin
	// bridged by another transport). Keep gRPC semantics otherwise.
	return r.NetRPCDecode(err)
}

func envelopeToStruct(env wireEnvelope) (*structpb.Struct, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, err
	}
	return structpb.NewStruct(map[string]any{structKeyEnvelope: nested})
}

func envelopeFromStruct(s *structpb.Struct) (wireEnvelope, bool) {
	var env wireEnvelope
	if s == nil {
		return env, false
	}
	nested, ok := s.AsMap()[structKeyEnvelope]
	if !ok {
		return env, false
	}
	raw, err := json.Marshal(nested)
	if err != nil {
		return env, false
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, false
	}
	return env, true
}
