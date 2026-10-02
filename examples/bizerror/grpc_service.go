// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"

	"google.golang.org/grpc"
)

// This file wires the tiny demo service onto a *grpc.Server without any
// generated stubs. It uses grpc.ServiceDesc directly with dynamically
// encoded messages; the demo only cares about error propagation.

const failMethod = failServiceName + "/Fail"

// registerFailureService registers a service exposing one unary method:
//
//	rpc Fail(FailRequest{index}) returns (FailResponse{});
//
// Messages use google.protobuf.Empty-compatible dynamic codec payloads of
// the well-known Struct type so no code generation is needed for the demo.
func registerFailureService(srv *grpc.Server, svc *grpcService) {
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "demo.FailureService",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Fail",
				Handler:    failHandler(svc),
			},
		},
		Streams:  []grpc.StreamDesc{},
		Metadata: "bizerror_demo.proto",
	}, svc)
}

func failHandler(svc *grpcService) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := &failRequest{}
		if err := dec(in); err != nil {
			return nil, err
		}
		if interceptor == nil {
			return &failResponse{}, svc.fail(ctx, in.Index)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: failMethod}
		handler := func(ctx context.Context, req any) (any, error) {
			return &failResponse{}, svc.fail(ctx, req.(*failRequest).Index)
		}
		return interceptor(ctx, in, info, handler)
	}
}

type failRequest struct {
	Index int `json:"index"`
}

type failResponse struct{}

// invokeFail calls the Fail method using JSON payloads. Both sides accept
// the application/grpc+json codec registered in grpc_json.go.
func invokeFail(ctx context.Context, cc *grpc.ClientConn, index int) error {
	return cc.Invoke(ctx, failMethod, &failRequest{Index: index}, &failResponse{})
}
