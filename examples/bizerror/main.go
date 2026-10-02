// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

// Command bizerror demonstrates the optional structured business error
// protocol over both net/rpc and gRPC. It runs entirely in one process
// (using in-memory listeners) and prints how the host classifies every
// scenario for each transport.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/hashicorp/go-plugin/bizerror"
	"github.com/hashicorp/go-plugin/examples/bizerror/shared"
)

func main() {
	fmt.Println("== net/rpc ==")
	runNetRPC()
	fmt.Println()
	fmt.Println("== gRPC ==")
	runGRPC()
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

// scenario is one error produced by a "plugin".
type scenario struct {
	name string
	err  error
}

func scenarios() []scenario {
	notFound := &shared.NotFoundError{
		Resource: "document/42",
		Key:      "42",
		Token:    "secret-token-AAA", // must never cross the boundary
	}

	unavailable := &shared.UnavailableError{
		Resource:      "document/42",
		RetryAfterSec: 30,
		InternalTrace: "db=10.0.0.1", // must never cross the boundary
	}

	denied := &shared.AccessDeniedError{Resource: "document/42", Reason: "policy/ro"}

	// Multiple causes: unavailable wraps a not-found and an access denial.
	multiCause := withCauses{
		msg:    "exhausted all replicas",
		causes: []error{notFound, denied},
	}

	// A truly cyclic graph: cyc1 -> cyc2 -> cyc1.
	cyc1 := &nodeError{msg: "cycle-1"}
	cyc2 := &nodeError{msg: "cycle-2", causes: []error{cyc1}}
	cyc1.causes = []error{cyc2}

	// A type the plugin was never told about: the host registered
	// HostOnly, the plugin did not. A plugin can still emit the agreed
	// type id as a generic *bizerror.Error.
	unknownToPlugin := &bizerror.Error{
		TypeID:  shared.TypeHostOnly,
		Message: "host-only error type",
		Known:   false,
	}

	// A type neither side registered, to show pure id-based handling.
	neither := &bizerror.Error{
		TypeID:  "store.v9.experimental",
		Message: "experimental error from a newer plugin",
		Fields:  map[string]any{"hint": "retry later"},
	}

	// Plain old error: an "old plugin" that never calls the encoders.
	plain := errors.New("legacy plugin: boom")

	return []scenario{
		{"registered typed error", notFound},
		{"registered with exported fields", unavailable},
		{"encode/decode hook", denied},
		{"multiple causes", &withCauses{msg: unavailable.Error(), causes: []error{unavailable, multiCause}}},
		{"cyclic references", cyc1},
		{"unknown type to receiver", unknownToPlugin},
		{"unregistered on both sides", neither},
		{"old plugin plain error", plain},
	}
}

type withCauses struct {
	msg    string
	causes []error
}

func (w withCauses) Error() string   { return w.msg }
func (w withCauses) Unwrap() []error { return w.causes }

type nodeError struct {
	msg    string
	causes []error
}

func (n *nodeError) Error() string   { return n.msg }
func (n *nodeError) Unwrap() []error { return n.causes }

// ---------------------------------------------------------------------------
// Host-side classification
// ---------------------------------------------------------------------------

func classify(name string, err error) {
	fmt.Printf("-- %s\n", name)
	if err == nil {
		fmt.Println("   nil error")
		return
	}

	var nf *shared.NotFoundError
	var un *shared.UnavailableError
	var ad *shared.AccessDeniedError
	var ho *shared.HostOnlyError
	var be *bizerror.Error

	switch {
	case errors.As(err, &nf):
		fmt.Printf("   class      : NotFound (errors.As)\n")
		fmt.Printf("   resource   : %q key=%q token=%q\n", nf.Resource, nf.Key, nf.Token)
	case errors.As(err, &un):
		fmt.Printf("   class      : Unavailable (errors.As) retry=%ds\n", un.RetryAfterSec)
		fmt.Printf("   trace leak : %q (should be empty)\n", un.InternalTrace)
	case errors.As(err, &ad):
		fmt.Printf("   class      : AccessDenied via hook: %s/%s\n", ad.Resource, ad.Reason)
	case errors.As(err, &ho):
		fmt.Printf("   class      : HostOnly reconstructed on host; plugin did not know this type\n")
	case errors.As(err, &be):
		fmt.Printf("   class      : generic business error type=%q known=%v\n", be.TypeID, be.Known)
		if errors.Is(err, bizerror.ErrUnknownType) {
			fmt.Println("   marker     : errors.Is(ErrUnknownType) = true")
		}
	default:
		fmt.Printf("   class      : PLAIN (no business type): %s\n", strings.TrimSpace(err.Error()))
	}

	if errors.Is(err, bizerror.ErrLimitExceeded) {
		fmt.Println("   marker     : ErrLimitExceeded present")
	}
	if errors.Is(err, bizerror.ErrCyclicReference) {
		fmt.Println("   marker     : cycle was broken (ErrCyclicReference)")
	}
	if errors.Is(err, bizerror.ErrBadReference) {
		fmt.Println("   marker     : ErrBadReference present")
	}

	// Walk the reconstructed tree one level to show cause structure.
	var be2 *bizerror.Error
	if errors.As(err, &be2) && be2.CauseCount() > 0 {
		fmt.Printf("   causes     : %d direct\n", be2.CauseCount())
	}

	// errors.Is against a generic type id also works without the concrete
	// Go type registered.
	if errors.Is(err, &bizerror.Error{TypeID: shared.TypeNotFound}) {
		fmt.Println("   errors.Is  : matches store.v1.not_found by type id")
	}
}

// ---------------------------------------------------------------------------
// net/rpc
// ---------------------------------------------------------------------------

type rpcService struct{ registry *bizerror.Registry }

type RPCFailRequest struct{ Index int }
type RPCFailResponse struct{}

func (s *rpcService) Fail(args RPCFailRequest, reply *RPCFailResponse) error {
	return s.registry.NetRPCEncode(scenarios()[args.Index].err)
}

func runNetRPC() {
	pluginReg := shared.PluginRegistry()
	hostReg := shared.HostRegistry()

	server := rpc.NewServer()
	if err := server.RegisterName("Plugin", &rpcService{registry: pluginReg}); err != nil {
		panic(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		panic(err)
	}
	client := rpc.NewClient(conn)
	defer client.Close()

	sc := scenarios()
	for i, s := range sc {
		err := hostReg.NetRPCDecode(client.Call("Plugin.Fail", RPCFailRequest{Index: i}, &RPCFailResponse{}))
		classify(s.name, err)
	}

	// Simulate an old plugin: it returns a normal error that was never
	// encoded. Calling such a server with the decoding client must produce a
	// plain error.
	err = hostReg.NetRPCDecode(client.Call("Plugin.MissingMethod", RPCFailRequest{}, &RPCFailResponse{}))
	classify("connection/method error stays plain", err)
}

// ---------------------------------------------------------------------------
// gRPC
// ---------------------------------------------------------------------------

const failServiceName = "/demo.FailureService"

type grpcService struct{ reg *bizerror.Registry }

func (s *grpcService) fail(ctx context.Context, idx int) error {
	return scenarios()[idx].err
}

func runGRPC() {
	pluginReg := shared.PluginRegistry()
	hostReg := shared.HostRegistry()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(pluginReg.GRPCServerInterceptor()),
	)
	registerFailureService(srv, &grpcService{reg: pluginReg})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(hostReg.GRPCClientInterceptor()),
	)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	sc := scenarios()
	for i, s := range sc {
		err := invokeFail(context.Background(), conn, i)
		classify(s.name, err)
	}

	// Unknown method: stays a regular gRPC status error (codes.Unknown).
	err = conn.Invoke(context.Background(), "/demo.DoesNotExist/Noop", nil, nil)
	classify("connection/method error stays plain", err)
	if status.Code(err) == codes.Unknown || status.Code(err) == codes.Unimplemented {
		fmt.Printf("   grpc code  : %s (preserved)\n", status.Code(err))
	}
}
