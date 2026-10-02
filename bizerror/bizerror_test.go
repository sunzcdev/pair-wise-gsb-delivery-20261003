// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// scenarioHolder hands the gRPC test server the error to return for the
// in-flight call. The empty request cannot carry the index, so the server
// reads the scenario under test from here.
var scenarioHolder struct {
	sync.Mutex
	err error
}

func setScenario(err error) {
	scenarioHolder.Lock()
	scenarioHolder.err = err
	scenarioHolder.Unlock()
}

// ---- shared test types -----------------------------------------------------

type notFound struct {
	Resource string
	Key      string
	Token    string // sensitive; never exported
}

func (e *notFound) Error() string { return "not found: " + e.Resource }

type unavailable struct {
	Resource      string
	RetryAfterSec int
	Trace         string // sensitive
}

func (e *unavailable) Error() string { return "unavailable: " + e.Resource }

type denied struct {
	Resource string
	Reason   string
}

func (e *denied) Error() string { return "denied: " + e.Resource }

// plainValue is an error on a value receiver, used to verify Prototype must
// return a pointer to a struct.
type plainValue struct{}

func (plainValue) Error() string { return "plain" }

type multi struct {
	msg    string
	causes []error
}

func (m *multi) Error() string   { return m.msg }
func (m *multi) Unwrap() []error { return m.causes }

type node struct {
	msg    string
	causes []error
}

func (n *node) Error() string   { return n.msg }
func (n *node) Unwrap() []error { return n.causes }

func testRegistry() *Registry {
	reg := NewRegistry()
	reg.MustRegister(Registration{
		TypeID:         "test.not_found",
		Prototype:      func() error { return &notFound{} },
		ExportedFields: []string{"Resource", "Key"},
	})
	reg.MustRegister(Registration{
		TypeID:         "test.unavailable",
		Prototype:      func() error { return &unavailable{} },
		ExportedFields: []string{"Resource", "RetryAfterSec"},
	})
	reg.MustRegister(Registration{
		TypeID:         "test.denied",
		ExportedFields: []string{"resource", "reason"},
		Encode: func(err error) (map[string]any, error) {
			e, ok := err.(*denied)
			if !ok {
				return nil, nil
			}
			return map[string]any{"resource": e.Resource, "reason": e.Reason}, nil
		},
		Decode: func(fields map[string]any) (error, error) {
			e := &denied{}
			e.Resource, _ = fields["resource"].(string)
			e.Reason, _ = fields["reason"].(string)
			return e, nil
		},
	})
	return reg
}

// ---- registry --------------------------------------------------------------

func TestRegisterValidation(t *testing.T) {
	reg := NewRegistry()

	if err := reg.Register(Registration{}); err == nil {
		t.Fatal("expected error for empty TypeID")
	}
	if err := reg.Register(Registration{TypeID: "x"}); err == nil {
		t.Fatal("expected error when neither Prototype nor Encode set")
	}
	if err := reg.Register(Registration{
		TypeID:    "x",
		Prototype: func() error { return plainValue{} }, // value type, not *struct
	}); err == nil {
		t.Fatal("expected error for non-pointer Prototype")
	}

	reg.MustRegister(Registration{
		TypeID:         "x",
		Prototype:      func() error { return &denied{} },
		ExportedFields: []string{"Resource"},
	})
	if err := reg.Register(Registration{
		TypeID:    "x",
		Prototype: func() error { return &denied{} },
	}); err == nil {
		t.Fatal("expected duplicate TypeID error")
	}
}

// ---- net/rpc round trips ---------------------------------------------------

type RPCArgs struct{ Index int }
type RPCReply struct{}

type rpcSvc struct{ registry *Registry }

func (s *rpcSvc) Fail(args RPCArgs, _ *RPCReply) error {
	return s.registry.NetRPCEncode(testErrors()[args.Index])
}

// testErrors returns the canonical scenario set shared by every transport.
func testErrors() []error {
	nf := &notFound{Resource: "doc/1", Key: "1", Token: "SECRET"}
	un := &unavailable{Resource: "doc/1", RetryAfterSec: 7, Trace: "INTERNAL"}
	dn := &denied{Resource: "doc/1", Reason: "policy"}

	a := &node{msg: "a"}
	b := &node{msg: "b", causes: []error{a}}
	a.causes = []error{b}

	return []error{
		nf,
		un,
		dn,
		&multi{msg: "fan-out", causes: []error{nf, dn}},
		a,
		&Error{TypeID: "test.unknown_to_host", Message: "future type", Fields: map[string]any{"x": 1}},
		errors.New("plain legacy failure"),
		fmt.Errorf("wrapped: %w", nf), // business error nested under a plain wrapper
	}
}

func dialNetRPC(t *testing.T, reg *Registry) *rpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := rpc.NewServer()
	if err := srv.RegisterName("Plugin", &rpcSvc{registry: reg}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go srv.ServeConn(c)
		}
	}()
	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return rpc.NewClient(conn)
}

func TestNetRPCScenarios(t *testing.T) {
	reg := testRegistry()
	client := dialNetRPC(t, reg)

	errs := testErrors()
	for i := range errs {
		var reply RPCReply
		err := reg.NetRPCDecode(client.Call("Plugin.Fail", RPCArgs{Index: i}, &reply))
		assertScenario(t, i, err)
	}
}

// ---- gRPC round trips ------------------------------------------------------

type grpcIface interface{}

func dialGRPC(t *testing.T, reg *Registry) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(reg.GRPCServerInterceptor()))
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Svc",
		HandlerType: (*grpcIface)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Fail",
			Handler: func(srv any, ctx context.Context, dec func(any) error, ci grpc.UnaryServerInterceptor) (any, error) {
				var req emptypb.Empty
				if err := dec(&req); err != nil {
					return nil, err
				}
				handler := func(ctx context.Context, _ any) (any, error) {
					scenarioHolder.Lock()
					err := scenarioHolder.err
					scenarioHolder.Unlock()
					return &emptypb.Empty{}, err
				}
				return ci(ctx, &emptypb.Empty{}, &grpc.UnaryServerInfo{FullMethod: "/test.Svc/Fail"}, handler)
			},
		}},
	}, nil)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(reg.GRPCClientInterceptor()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func TestGRPCScenarios(t *testing.T) {
	reg := testRegistry()
	cc := dialGRPC(t, reg)

	for i, e := range testErrors() {
		setScenario(e)
		var reply emptypb.Empty
		err := cc.Invoke(context.Background(), "/test.Svc/Fail", &emptypb.Empty{}, &reply)
		assertScenario(t, i, err)
	}
}

// ---- shared assertions -----------------------------------------------------

func assertScenario(t *testing.T, i int, err error) {
	t.Helper()
	switch i {
	case 0: // not found, reflected fields, secret stripped
		var got *notFound
		if !errors.As(err, &got) {
			t.Fatalf("[%d] want *notFound, got %T: %v", i, err, err)
		}
		if got.Resource != "doc/1" || got.Key != "1" {
			t.Fatalf("[%d] fields not reconstructed: %+v", i, got)
		}
		if got.Token != "" {
			t.Fatalf("[%d] sensitive Token leaked: %q", i, got.Token)
		}
	case 1: // unavailable
		var got *unavailable
		if !errors.As(err, &got) || got.RetryAfterSec != 7 {
			t.Fatalf("[%d] want *unavailable retry=7, got %T %v", i, err, err)
		}
		if got.Trace != "" {
			t.Fatalf("[%d] Trace leaked: %q", i, got.Trace)
		}
	case 2: // denied via hook
		var got *denied
		if !errors.As(err, &got) || got.Reason != "policy" {
			t.Fatalf("[%d] want *denied via hook, got %T %v", i, err, err)
		}
	case 3: // multi cause tree
		var nf *notFound
		var dn *denied
		if !errors.As(err, &nf) {
			t.Fatalf("[%d] want notFound in causes, got %v", i, err)
		}
		if !errors.As(err, &dn) {
			t.Fatalf("[%d] want denied in causes, got %v", i, err)
		}
	case 4: // cyclic: must terminate and carry the cycle marker
		if !errors.Is(err, ErrCyclicReference) {
			t.Fatalf("[%d] want ErrCyclicReference, got %v", i, err)
		}
		// A bounded traversal must terminate.
		if depth := traverse(err, 0); depth < 0 {
			t.Fatalf("[%d] reconstructed graph still cyclic", i)
		}
	case 5: // unknown type on the receiving side
		var be *Error
		if !errors.As(err, &be) {
			t.Fatalf("[%d] want *Error, got %T", i, err)
		}
		if be.Known {
			t.Fatalf("[%d] expected Known=false", i)
		}
		if !errors.Is(err, ErrUnknownType) {
			t.Fatalf("[%d] want ErrUnknownType marker", i)
		}
		if be.TypeID != "test.unknown_to_host" {
			t.Fatalf("[%d] type id lost: %q", i, be.TypeID)
		}
		// id-based Is works even without the concrete type.
		if !errors.Is(err, &Error{TypeID: "test.unknown_to_host"}) {
			t.Fatalf("[%d] errors.Is by type id failed", i)
		}
	case 6: // old plugin / plain error stays a plain string error
		var be *Error
		if errors.As(err, &be) {
			t.Fatalf("[%d] plain error must not become a business error", i)
		}
		if !strings.Contains(err.Error(), "plain legacy failure") {
			t.Fatalf("[%d] message changed: %v", i, err)
		}
	case 7: // wrapped business error recovered from inside a plain wrapper
		var nf *notFound
		if !errors.As(err, &nf) {
			t.Fatalf("[%d] business error nested in wrapper not recovered, got %v", i, err)
		}
	}
}

// traverse returns the max depth reached, or -1 if it detects a revisit that
// would loop beyond a safe bound.
func traverse(err error, depth int) int {
	if depth > MaxDepth+5 {
		return -1
	}
	max := depth
	for _, c := range causesOf(err) {
		if d := traverse(c, depth+1); d < 0 {
			return -1
		} else if d > max {
			max = d
		}
	}
	return max
}

// ---- connection / transport errors stay classified ------------------------

func TestNetRPCConnectionErrorUntouched(t *testing.T) {
	reg := testRegistry()
	client := dialNetRPC(t, reg)
	var reply RPCReply
	err := reg.NetRPCDecode(client.Call("Plugin.DoesNotExist", RPCArgs{}, &reply))
	var be *Error
	if errors.As(err, &be) {
		t.Fatal("missing method error must not be a business error")
	}
}

func TestGRPCConnectionErrorUntouched(t *testing.T) {
	reg := testRegistry()
	cc := dialGRPC(t, reg)
	setScenario(errors.New("x"))

	err := cc.Invoke(context.Background(), "/test.DoesNotExist/Noop", &emptypb.Empty{}, &emptypb.Empty{})
	code := status.Code(err)
	if code != codes.Unimplemented && code != codes.Unknown {
		t.Fatalf("want a preserved gRPC code, got %v", code)
	}
	var be *Error
	if errors.As(err, &be) {
		t.Fatal("transport status error must not become a business error")
	}
}
