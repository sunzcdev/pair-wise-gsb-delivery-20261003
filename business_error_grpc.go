// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const businessErrorMetadata = "go-plugin-business-error-v1-bin"

func unaryBusinessErrorServerInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		return resp, nil
	}

	if data, ok := encodeBusinessError(err); ok {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(businessErrorMetadata, string(data)))
	}
	return resp, err
}

func unaryBusinessErrorClientInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	var trailer metadata.MD
	opts = append(opts, grpc.Trailer(&trailer))
	err := invoker(ctx, method, req, reply, cc, opts...)
	if err == nil {
		return nil
	}

	values := trailer.Get(businessErrorMetadata)
	if len(values) == 0 {
		return err
	}
	if decoded := decodeBusinessError([]byte(values[0])); decoded != nil {
		return decoded
	}
	return err
}
