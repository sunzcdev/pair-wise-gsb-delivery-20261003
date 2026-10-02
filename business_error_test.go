// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"testing"

	grpctest "github.com/hashicorp/go-plugin/test/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type unavailableKind struct{}

func (unavailableKind) Error() string { return "temporarily unavailable" }

type businessErrorTestService struct {
	grpctest.UnimplementedPingPongServer
	err error
}

func (s *businessErrorTestService) Ping(context.Context, *grpctest.PingRequest) (*grpctest.PongResponse, error) {
	return nil, s.err
}

func registerBusinessErrorTestTypes(t *testing.T) (*BusinessErrorType[error], *BusinessErrorType[unavailableKind]) {
	t.Helper()

	notFound := RegisterBusinessError[error]("test/resource-not-found", map[string]bool{
		"key":      true,
		"token":    false,
		"internal": false,
	})
	unavailable := RegisterBusinessError[unavailableKind]("test/temporarily-unavailable", map[string]bool{
		"retry_after_seconds": true,
		"upstream_token":      false,
	})
	return notFound, unavailable
}

func TestBusinessError_GRPCRegisteredType(t *testing.T) {
	notFound, unavailable := registerBusinessErrorTestTypes(t)

	cause1 := notFound.New("key a is unavailable", map[string]any{"key": "a"})
	cause2 := notFound.New("key b is unavailable", map[string]any{"key": "b", "token": "secret"})
	err := unavailable.New("cache warming", map[string]any{
		"retry_after_seconds": 5,
		"upstream_token":      "secret",
	}, cause1, cause2)

	client, server := testBusinessErrorGRPCConn(t, err, true)
	defer func() { _ = client.Close() }()
	defer server.Stop()

	_, got := grpctest.NewPingPongClient(client).Ping(context.Background(), &grpctest.PingRequest{})
	if got == nil {
		t.Fatal("expected error")
	}

	typed, ok := unavailable.As(got)
	if !ok {
		t.Fatalf("expected registered type: %#v", got)
	}
	if !unavailable.Is(got) {
		t.Fatal("errors.Is-style check failed")
	}
	if value, ok := typed.Field("retry_after_seconds"); !ok || value.(json.Number).String() != "5" {
		t.Fatalf("unexpected retry field: %#v %t", value, ok)
	}
	if value, ok := typed.Field("upstream_token"); ok {
		t.Fatalf("sensitive field restored on receiver: %#v", value)
	}
	if len(typed.Unwrap()) != 2 {
		t.Fatalf("expected two causes: %#v", typed.Unwrap())
	}
	if !notFound.Is(got) {
		t.Fatal("cause was not restored")
	}
}

func TestBusinessError_UnknownType(t *testing.T) {
	registerBusinessErrorTestTypes(t)

	envelope := businessErrorEnvelope{
		Version: 1,
		Nodes: []businessErrorWireNode{{
			Type:   "test/unknown-to-receiver",
			Text:   "newer plugin error",
			Fields: map[string]any{"key": "value"},
		}},
	}
	data, err := json.Marshal(&envelope)
	if err != nil {
		t.Fatal(err)
	}

	decoded := decodeBusinessError(data)
	var remote *RemoteBusinessError
	if !errors.As(decoded, &remote) || remote.ID() != "test/unknown-to-receiver" {
		t.Fatalf("expected remote unknown type: %#v", decoded)
	}
	if value, ok := remote.Field("key"); !ok || value != "value" {
		t.Fatalf("unknown fields not retained: %#v %t", value, ok)
	}
}

func TestBusinessError_GRPCLegacyAndConnectionErrors(t *testing.T) {
	conn, server := testBusinessErrorGRPCConn(t, errors.New("plain legacy plugin error"), false)

	pingClient := grpctest.NewPingPongClient(conn)
	_, legacyErr := pingClient.Ping(context.Background(), &grpctest.PingRequest{})
	if status.Code(legacyErr) != codes.Unknown || legacyErr.Error() != "rpc error: code = Unknown desc = plain legacy plugin error" {
		t.Fatalf("legacy classification changed: %#v", legacyErr)
	}

	server.Stop()
	_ = conn.Close()
	_, closedErr := pingClient.Ping(context.Background(), &grpctest.PingRequest{})
	if closedErr == nil {
		t.Fatal("expected connection error")
	}
	var remote *RemoteBusinessError
	if errors.As(closedErr, &remote) {
		t.Fatalf("connection error classified as business error: %#v", closedErr)
	}
}

func TestBusinessError_NetRPCRegisteredType(t *testing.T) {
	notFound, unavailable := registerBusinessErrorTestTypes(t)

	client, serverConn := TestConn(t)
	server := rpc.NewServer()
	if err := server.RegisterName("Plugin", &netRPCBusinessErrorServer{
		err: unavailable.New("temporary", map[string]any{"retry_after_seconds": 1}, notFound.New("missing", map[string]any{
			"key":   "alpha",
			"token": "secret",
		})),
	}); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)
	defer func() { _ = client.Close() }()

	rpcClient := rpc.NewClient(client)
	var response struct{}
	err := DecodeRPCError(rpcClient.Call("Plugin.Fail", struct{}{}, &response))

	typed, ok := unavailable.As(err)
	if !ok {
		t.Fatalf("expected registered type: %#v", err)
	}
	if value, ok := typed.Field("retry_after_seconds"); !ok || value.(json.Number).String() != "1" {
		t.Fatalf("unexpected field: %#v", value)
	}
	if value, ok := typed.Field("upstream_token"); ok {
		t.Fatalf("sensitive field crossed the boundary: %#v", value)
	}
	if !notFound.Is(err) {
		t.Fatal("cause was not restored")
	}
}

func TestBusinessError_SensitiveFieldsAndWrappedRoot(t *testing.T) {
	notFound, _ := registerBusinessErrorTestTypes(t)
	local := notFound.New("missing", map[string]any{
		"key":   "local-key",
		"token": "local-secret",
	})
	if value, ok := local.Field("token"); !ok || value != "local-secret" {
		t.Fatalf("sensitive field should remain local: %#v %t", value, ok)
	}

	wrapped := fmt.Errorf("operation failed: %w", local)
	data, ok := encodeBusinessError(wrapped)
	if !ok {
		t.Fatal("wrapped registered error did not encode")
	}
	decoded := decodeBusinessError(data)
	if !notFound.Is(decoded) {
		t.Fatalf("wrapped registered error was not restored: %#v", decoded)
	}
	var typed *BusinessError[error]
	if !errors.As(decoded, &typed) {
		t.Fatal("typed business error not found")
	}
	if typed.ID() != notFound.ID() {
		t.Fatalf("registered root was not promoted: %q", typed.ID())
	}
	if _, ok := typed.Field("token"); ok {
		t.Fatal("sensitive field was exported")
	}
}

func TestBusinessError_NetRPCLegacyAndConnectionErrors(t *testing.T) {
	client, serverConn := TestConn(t)
	server := rpc.NewServer()
	if err := server.RegisterName("Plugin", &netRPCBusinessErrorServer{
		err: errors.New("plain net/rpc error"),
	}); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)

	rpcClient := rpc.NewClient(client)
	var response struct{}
	legacyErr := DecodeRPCError(rpcClient.Call("Plugin.Fail", struct{}{}, &response))
	var serverErr rpc.ServerError
	if !errors.As(legacyErr, &serverErr) || serverErr.Error() != "plain net/rpc error" {
		t.Fatalf("legacy classification changed: %#v", legacyErr)
	}

	_ = client.Close()
	closedErr := DecodeRPCError(rpcClient.Call("Plugin.Fail", struct{}{}, &response))
	if !errors.Is(closedErr, rpc.ErrShutdown) {
		t.Fatalf("connection classification changed: %#v", closedErr)
	}
}

func TestBusinessError_CyclicReference(t *testing.T) {
	notFound, unavailable := registerBusinessErrorTestTypes(t)

	a := unavailable.New("a", nil)
	b := notFound.New("b", nil, a)
	a.meta.causes = []error{b}

	data, ok := encodeBusinessError(a)
	if !ok {
		t.Fatal("expected encoding")
	}
	decoded := decodeBusinessError(data)
	if !unavailable.Is(decoded) || !notFound.Is(decoded) {
		t.Fatalf("cyclic graph types not restored: %#v", decoded)
	}

	visited := map[error]bool{}
	if walkBusinessErrors(decoded, visited, 0) {
		t.Fatal("decoded error graph traversal cycles")
	}
}

type netRPCBusinessErrorServer struct {
	err error
}

func testBusinessErrorGRPCConn(t *testing.T, err error, businessErrors bool) (*grpc.ClientConn, *grpc.Server) {
	t.Helper()

	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatal(listenErr)
	}

	var opts []grpc.ServerOption
	if businessErrors {
		opts = append(opts, grpc.ChainUnaryInterceptor(unaryBusinessErrorServerInterceptor))
	}
	server := grpc.NewServer(opts...)
	grpctest.RegisterPingPongServer(server, &businessErrorTestService{err: err})
	go func() { _ = server.Serve(listener) }()

	dialOpts := []grpc.DialOption{grpc.WithChainUnaryInterceptor(unaryBusinessErrorClientInterceptor)}
	dialOpts = append(dialOpts,
		grpc.WithBlock(), //nolint:staticcheck
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, dialErr := grpc.Dial(listener.Addr().String(), dialOpts...) //nolint:staticcheck
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	_ = listener.Close()
	return conn, server
}

func (s *netRPCBusinessErrorServer) Fail(struct{}, *struct{}) error {
	return EncodeRPCError(s.err)
}

func walkBusinessErrors(err error, visited map[error]bool, depth int) bool {
	if err == nil || depth > maxBusinessErrorDepth {
		return depth > maxBusinessErrorDepth
	}
	if visited[err] {
		return true
	}
	visited[err] = true
	for _, cause := range unwrapBusinessErrors(err) {
		if walkBusinessErrors(cause, visited, depth+1) {
			return true
		}
	}
	return false
}
