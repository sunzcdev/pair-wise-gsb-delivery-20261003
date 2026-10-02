package sasl_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
)

// hangingClient is a deliberately non-completing mechanism: each call blocks
// on release until release is closed, or never returns when release is left
// open. Calls count so tests can assert the underlying mechanism is not
// re-entered once the exchange is terminal.
type hangingClient struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once

	startCalls int
	nextCalls  int

	ir     []byte
	resp   []byte
	startE error
	nextE  error
}

func newHangingClient() *hangingClient {
	return &hangingClient{
		release: make(chan struct{}),
		entered: make(chan struct{}),
		ir:      []byte("client-initial-response"),
		resp:    []byte("client-response"),
	}
}

func (c *hangingClient) signalEntered() {
	c.once.Do(func() { close(c.entered) })
}

func (c *hangingClient) wait() {
	<-c.release
}

func (c *hangingClient) Start() (mech string, ir []byte, err error) {
	c.startCalls++
	c.signalEntered()
	c.wait()
	return "HANGING", c.ir, c.startE
}

func (c *hangingClient) Next(challenge []byte) (response []byte, err error) {
	c.nextCalls++
	c.signalEntered()
	c.wait()
	return c.resp, c.nextE
}

// hangingServer is the server-side deliberately non-completing mechanism.
type hangingServer struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once

	calls int

	challenge []byte
	done      bool
	err       error
}

func newHangingServer() *hangingServer {
	return &hangingServer{
		release:   make(chan struct{}),
		entered:   make(chan struct{}),
		challenge: []byte("server-challenge"),
	}
}

func (s *hangingServer) signalEntered() {
	s.once.Do(func() { close(s.entered) })
}

func (s *hangingServer) wait() {
	<-s.release
}

func (s *hangingServer) Next(response []byte) (challenge []byte, done bool, err error) {
	s.calls++
	s.signalEntered()
	s.wait()
	return s.challenge, s.done, s.err
}

// staticClient returns canned values without blocking.
type staticClient struct {
	mech string
	ir   []byte
	resp []byte

	startCalls int
	nextCalls  int
	startErr   error
	nextErr    error
}

func (c *staticClient) Start() (mech string, ir []byte, err error) {
	c.startCalls++
	return c.mech, c.ir, c.startErr
}

func (c *staticClient) Next(challenge []byte) (response []byte, err error) {
	c.nextCalls++
	return c.resp, c.nextErr
}

// staticServer returns canned values without blocking.
type staticServer struct {
	challenge []byte
	done      bool
	err       error

	calls int
}

func (s *staticServer) Next(response []byte) (challenge []byte, done bool, err error) {
	s.calls++
	return s.challenge, s.done, s.err
}

func budgetErr(t *testing.T, err error) *sasl.BudgetError {
	t.Helper()
	var be *sasl.BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("expected *sasl.BudgetError, got %T: %v", err, err)
	}
	return be
}

func assertBudgetLimit(t *testing.T, err error, want sasl.BudgetLimit) *sasl.BudgetError {
	t.Helper()
	be := budgetErr(t, err)
	if be.Limit != want {
		t.Fatalf("expected limit %v, got %v (%v)", want, be.Limit, err)
	}
	return be
}

func TestBudgetClientNilVsEmptyInitialResponse(t *testing.T) {
	t.Run("nil ir preserved", func(t *testing.T) {
		inner := &staticClient{mech: "X", ir: nil}
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{}, inner)
		mech, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		if mech != "X" {
			t.Fatalf("unexpected mechanism %q", mech)
		}
		if ir != nil {
			t.Fatalf("expected nil initial response, got %v", ir)
		}
	})

	t.Run("zero-length ir preserved", func(t *testing.T) {
		inner := &staticClient{mech: "X", ir: []byte{}}
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{}, inner)
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		if ir == nil {
			t.Fatal("expected non-nil zero-length initial response")
		}
		if len(ir) != 0 {
			t.Fatalf("expected zero-length initial response, got %v", ir)
		}
	})
}

func TestBudgetServerNilVsEmptyResponse(t *testing.T) {
	server := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{}, &staticServer{challenge: nil, done: true})
	challenge, done, err := server.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("expected exchange to be done")
	}
	if challenge != nil {
		t.Fatalf("expected nil challenge, got %v", challenge)
	}

	inner := &staticServer{challenge: []byte{}, done: false}
	server = sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{}, inner)
	challenge, done, err = server.Next([]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("expected exchange to continue")
	}
	if challenge == nil {
		t.Fatal("expected non-nil zero-length challenge")
	}
	if len(challenge) != 0 {
		t.Fatalf("expected zero-length challenge, got %v", challenge)
	}
}

func TestBudgetClientTerminalStateStartsAtStart(t *testing.T) {
	inner := newHangingClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := sasl.NewBudgetClient(ctx, sasl.ExchangeBudget{}, inner)

	mech, ir, err := c.Start()
	if err == nil {
		t.Fatal("expected budget error")
	}
	be := assertBudgetLimit(t, err, sasl.LimitCanceled)
	if be.Step != 1 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if mech != "" || ir != nil {
		t.Fatalf("expected no mechanism data, got %q %v", mech, ir)
	}
	if inner.startCalls != 0 {
		t.Fatalf("underlying client was entered %d times", inner.startCalls)
	}

	// The terminal state is stable and never enters the underlying mechanism.
	ir2, err2 := c.Next([]byte("anything"))
	if ir2 != nil || err2 != err {
		t.Fatal("expected the same stable terminal error and no response")
	}
	_, _, err3 := c.Start()
	if err3 != err {
		t.Fatal("expected the same stable terminal error from Start")
	}
	if inner.startCalls != 0 || inner.nextCalls != 0 {
		t.Fatal("underlying client was entered after terminal state")
	}
}

func TestBudgetClientStepsMultiStep(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("resp")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxSteps: 1}, inner)

	if _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Next([]byte("challenge"))
	be := assertBudgetLimit(t, err, sasl.LimitSteps)
	if be.Step != 2 || be.LimitValue != 1 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if inner.nextCalls != 0 {
		t.Fatalf("underlying Next entered %d times", inner.nextCalls)
	}
}

func TestBudgetServerSteps(t *testing.T) {
	inner := &staticServer{challenge: nil, done: true}
	s := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxSteps: 1}, inner)

	if _, _, err := s.Next([]byte("first")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Next([]byte("second"))
	be := assertBudgetLimit(t, err, sasl.LimitSteps)
	if be.Step != 2 || be.LimitValue != 1 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if inner.calls != 1 {
		t.Fatalf("expected underlying server to be entered once, got %d", inner.calls)
	}
}

func TestBudgetClientMessageLimit(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("resp")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxMessageBytes: 4}, inner)

	if _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Next([]byte("hello"))
	be := assertBudgetLimit(t, err, sasl.LimitMessage)
	if be.Size != 5 || be.LimitValue != 4 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if inner.nextCalls != 0 {
		t.Fatal("underlying Next must not be entered for oversized message")
	}

	// A message exactly at the limit is accepted.
	inner2 := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("ok")}
	c2 := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxMessageBytes: 4}, inner2)
	if _, _, err := c2.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Next([]byte("four")); err != nil {
		t.Fatalf("expected message at limit to be accepted, got %v", err)
	}
}

func TestBudgetServerMessageLimit(t *testing.T) {
	inner := &staticServer{challenge: nil, done: true}
	s := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxMessageBytes: 3}, inner)

	_, _, err := s.Next([]byte("toolon"))
	be := assertBudgetLimit(t, err, sasl.LimitMessage)
	if be.Size != 6 || be.LimitValue != 3 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if inner.calls != 0 {
		t.Fatal("underlying server must not be entered for oversized response")
	}
}

func TestBudgetClientCumulativeInput(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("r")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxInputBytes: 5}, inner)

	if _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	// 2 bytes already consumed; the next 4-byte challenge reaches 6 > 5.
	_, err := c.Next([]byte("cdef"))
	be := assertBudgetLimit(t, err, sasl.LimitInput)
	if be.Size != 6 || be.LimitValue != 5 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
	if inner.nextCalls != 1 {
		t.Fatalf("expected one underlying Next, got %d", inner.nextCalls)
	}

	// Zero-length challenges consume no input budget.
	inner2 := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("r")}
	c2 := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxInputBytes: 1}, inner2)
	if _, _, err := c2.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Next([]byte{}); err != nil {
		t.Fatalf("zero-length message must not consume input budget: %v", err)
	}
	if _, err := c2.Next(nil); err != nil {
		t.Fatalf("nil message must not consume input budget: %v", err)
	}
}

func TestBudgetServerCumulativeInput(t *testing.T) {
	inner := &staticServer{challenge: []byte{}, done: false}
	s := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxInputBytes: 5}, inner)

	if _, _, err := s.Next([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Next([]byte("cdef"))
	be := assertBudgetLimit(t, err, sasl.LimitInput)
	if be.Size != 6 || be.LimitValue != 5 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
}

func TestBudgetClientOutputTruncation(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("0123456789"), resp: []byte("abcdef")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxOutputBytes: 4}, inner)

	mech, ir, err := c.Start()
	be := assertBudgetLimit(t, err, sasl.LimitOutput)
	if mech != "X" {
		t.Fatalf("mechanism name still delivered, got %q", mech)
	}
	if string(ir) != "0123" {
		t.Fatalf("expected truncated prefix, got %q", ir)
	}
	if be.Delivered != 4 || be.Size != 10 || be.LimitValue != 4 {
		t.Fatalf("unexpected accounting: %+v", be)
	}

	// Terminal: the truncated bytes were already delivered, no re-entry.
	resp, err := c.Next([]byte("challenge"))
	if resp != nil || err != be {
		t.Fatal("expected stable terminal error without entering underlying client")
	}
	if inner.nextCalls != 0 {
		t.Fatal("underlying Next must not be entered after truncation")
	}
}

func TestBudgetClientOutputTruncationAcrossSteps(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ab"), resp: []byte("cdef")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxOutputBytes: 3}, inner)

	if _, ir, err := c.Start(); err != nil {
		t.Fatal(err)
	} else if string(ir) != "ab" {
		t.Fatalf("unexpected ir %q", ir)
	}

	// 2 of 3 output bytes remain; the 4-byte response is truncated to 1 byte.
	resp, err := c.Next([]byte("in"))
	be := assertBudgetLimit(t, err, sasl.LimitOutput)
	if string(resp) != "c" {
		t.Fatalf("expected truncated prefix of 1 byte, got %q", resp)
	}
	if be.Delivered != 1 || be.Size != 6 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
}

func TestBudgetServerOutputTruncation(t *testing.T) {
	inner := &staticServer{challenge: []byte("0123456789"), done: false}
	s := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxOutputBytes: 4}, inner)

	challenge, done, err := s.Next([]byte("request"))
	be := assertBudgetLimit(t, err, sasl.LimitOutput)
	if string(challenge) != "0123" {
		t.Fatalf("expected truncated challenge, got %q", challenge)
	}
	if done {
		t.Fatal("done flag from underlying exchange is passed through")
	}
	if be.Delivered != 4 {
		t.Fatalf("unexpected accounting: %+v", be)
	}
}

func TestBudgetClientCanceledBeforeStart(t *testing.T) {
	inner := newHangingClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := sasl.NewBudgetClient(ctx, sasl.ExchangeBudget{}, inner)

	_, _, err := c.Start()
	be := budgetErr(t, err)
	if be.Limit != sasl.LimitCanceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if !errors.Is(err, sasl.ErrExchangeCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected sentinel and context unwrap, got %v", err)
	}
	if inner.startCalls != 0 {
		t.Fatal("underlying client must not be entered after cancellation")
	}

	_, err = c.Next([]byte("x"))
	if err != be {
		t.Fatal("expected stable terminal cancellation error")
	}
}

func TestBudgetServerCanceledBeforeNext(t *testing.T) {
	inner := newHangingServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := sasl.NewBudgetServer(ctx, sasl.ExchangeBudget{}, inner)

	_, _, err := s.Next([]byte("x"))
	if be := budgetErr(t, err); be.Limit != sasl.LimitCanceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if inner.calls != 0 {
		t.Fatal("underlying server must not be entered after cancellation")
	}
}

func TestBudgetClientCanceledDuringExchange(t *testing.T) {
	inner := newHangingClient()
	ctx, cancel := context.WithCancel(context.Background())
	c := sasl.NewBudgetClient(ctx, sasl.ExchangeBudget{}, inner)

	started := make(chan struct{})
	go func() {
		_, _, _ = c.Start()
		close(started)
	}()

	// Wait until the call is actually parked inside the underlying mechanism.
	<-inner.entered

	// The in-flight call is parked inside the deliberately non-completing
	// mechanism; canceling does not interrupt it.
	cancel()
	select {
	case <-started:
		t.Fatal("in-flight call must not be interrupted by cancellation")
	default:
	}

	close(inner.release)
	<-started
	if inner.startCalls != 1 {
		t.Fatalf("expected one underlying Start, got %d", inner.startCalls)
	}

	// The next call observes the cancellation and never enters the mechanism.
	_, err := c.Next([]byte("challenge"))
	if be := budgetErr(t, err); be.Limit != sasl.LimitCanceled {
		t.Fatalf("expected cancellation on next step, got %v", err)
	}
	if inner.nextCalls != 0 {
		t.Fatal("underlying Next must not be entered after cancellation")
	}
}

func TestBudgetServerCanceledDuringExchange(t *testing.T) {
	inner := newHangingServer()
	ctx, cancel := context.WithCancel(context.Background())
	s := sasl.NewBudgetServer(ctx, sasl.ExchangeBudget{}, inner)

	finished := make(chan struct{})
	go func() {
		_, _, _ = s.Next([]byte("first"))
		close(finished)
	}()

	<-inner.entered

	cancel()
	close(inner.release)
	<-finished

	_, _, err := s.Next([]byte("second"))
	if be := budgetErr(t, err); be.Limit != sasl.LimitCanceled {
		t.Fatalf("expected cancellation on next step, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("underlying server must be entered exactly once, got %d", inner.calls)
	}
}

func TestBudgetClientContextDeadline(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ir")}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	c := sasl.NewBudgetClient(ctx, sasl.ExchangeBudget{}, inner)

	_, _, err := c.Start()
	be := budgetErr(t, err)
	if be.Limit != sasl.LimitCanceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline unwrap, got %v", err)
	}
}

func TestBudgetUnderlyingErrorDoesNotTerminate(t *testing.T) {
	underlyingErr := errors.New("boom")
	inner := &staticClient{
		mech:     "X",
		ir:       []byte("ir"),
		startErr: nil,
		nextErr:  underlyingErr,
	}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxSteps: 10}, inner)

	if _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next([]byte("challenge")); err != underlyingErr {
		t.Fatalf("expected underlying error, got %v", err)
	}
	// A mechanism error is not a budget terminal state: the exchange remains
	// usable and the step counter advances.
	inner.nextErr = nil
	if _, err := c.Next([]byte("again")); err != nil {
		t.Fatalf("expected exchange to remain usable, got %v", err)
	}
}

func TestBudgetErrorOmitsMessageData(t *testing.T) {
	inner := &staticClient{mech: "X", ir: []byte("ir"), resp: []byte("resp")}
	c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxMessageBytes: 3}, inner)
	if _, _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Next([]byte("SECRET-CHALLENGE"))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("budget error must not contain message data: %v", err)
	}
}

func TestBudgetLoginClientBoundaries(t *testing.T) {
	const password = "hunter2"

	t.Run("two-step exchange within budget succeeds", func(t *testing.T) {
		inner := sasl.NewLoginClient("alice", password)
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{
			MaxSteps:        2,
			MaxMessageBytes: len("Password:"),
			MaxInputBytes:   len("Password:"),
			MaxOutputBytes:  len("alice") + len(password),
		}, inner)

		mech, ir, err := c.Start()
		if err != nil {
			t.Fatal(err)
		}
		if mech != sasl.Login || string(ir) != "alice" {
			t.Fatalf("unexpected start: %q %q", mech, ir)
		}
		resp, err := c.Next([]byte("Password:"))
		if err != nil {
			t.Fatal(err)
		}
		if string(resp) != password {
			t.Fatalf("unexpected password response %q", resp)
		}
	})

	t.Run("one step is not enough for the second leg", func(t *testing.T) {
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxSteps: 1},
			sasl.NewLoginClient("alice", password))
		if _, _, err := c.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Next([]byte("Password:")); !errors.Is(err, sasl.ErrBudgetExceeded) {
			t.Fatalf("expected budget exceeded, got %v", err)
		}
	})

	t.Run("output truncated before the password is delivered", func(t *testing.T) {
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{
			MaxOutputBytes: len("alice") + 2,
		}, sasl.NewLoginClient("alice", password))

		if _, ir, err := c.Start(); err != nil {
			t.Fatal(err)
		} else if string(ir) != "alice" {
			t.Fatalf("unexpected ir %q", ir)
		}
		resp, err := c.Next([]byte("Password:"))
		be := assertBudgetLimit(t, err, sasl.LimitOutput)
		if string(resp) != password[:2] {
			t.Fatalf("expected truncated password prefix, got %q", resp)
		}
		if be.Delivered != 2 {
			t.Fatalf("expected 2 delivered bytes, got %d", be.Delivered)
		}
		if strings.Contains(err.Error(), password) {
			t.Fatalf("budget error leaks password: %v", err)
		}
	})
}

func TestBudgetOAuthBearerClientBoundaries(t *testing.T) {
	const token = "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB"

	t.Run("initial response truncated without leaking the token", func(t *testing.T) {
		inner := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "fxcp",
			Token:    token,
		})
		c := sasl.NewBudgetClient(context.Background(), sasl.ExchangeBudget{MaxOutputBytes: 8}, inner)

		mech, ir, err := c.Start()
		assertBudgetLimit(t, err, sasl.LimitOutput)
		if mech != sasl.OAuthBearer {
			t.Fatalf("unexpected mechanism %q", mech)
		}
		if len(ir) != 8 {
			t.Fatalf("expected 8 delivered bytes, got %d", len(ir))
		}
		if bytes.Contains(ir, []byte(token)) || strings.Contains(err.Error(), token) {
			t.Fatal("truncated output or error must not contain the bearer token")
		}
	})

	t.Run("invalid token multi-step exchange at the edge", func(t *testing.T) {
		authenticator := func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if opts.Token == token {
				return nil
			}
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}

		server := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxSteps: 2},
			sasl.NewOAuthBearerServer(authenticator))

		_, ir, err := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: "fxcp",
			Token:    "a-different-token",
		}).Start()
		if err != nil {
			t.Fatal(err)
		}

		challenge, done, err := server.Next(ir)
		if err != nil || done {
			t.Fatalf("expected JSON challenge and continued exchange, got %q done=%v err=%v", challenge, done, err)
		}

		// The second server step receives the client dummy response and reports
		// the stored authentication failure.
		_, _, err = server.Next([]byte{0x01})
		if err == nil {
			t.Fatal("expected authentication failure on the second step")
		}

		// A third step is refused by the budget before reaching the mechanism.
		_, _, err = server.Next([]byte{0x01})
		if !errors.Is(err, sasl.ErrBudgetExceeded) {
			t.Fatalf("expected stable budget exhaustion, got %v", err)
		}
	})

	t.Run("oversized client response rejected before parsing", func(t *testing.T) {
		server := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxMessageBytes: 4},
			sasl.NewOAuthBearerServer(func(sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
				return nil
			}))
		_, _, err := server.Next([]byte("n,a=fxcp,\x01auth=Bearer " + token + "\x01\x01"))
		if be := assertBudgetLimit(t, err, sasl.LimitMessage); strings.Contains(be.Error(), token) {
			t.Fatal("budget error leaks bearer token")
		}
	})
}

func TestBudgetPlainServerEndToEndBoundaries(t *testing.T) {
	const password = "server-password"
	authenticated := false
	authenticator := func(identity, username, pw string) error {
		authenticated = identity == "" && username == "user" && pw == password
		return nil
	}

	t.Run("nil initial response stays nil and costs nothing", func(t *testing.T) {
		server := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{MaxInputBytes: 0},
			sasl.NewPlainServer(authenticator))
		challenge, done, err := server.Next(nil)
		if err != nil || done {
			t.Fatalf("unexpected start: %q done=%v err=%v", challenge, done, err)
		}
		if challenge == nil || len(challenge) != 0 {
			t.Fatalf("expected non-nil zero-length challenge, got %v", challenge)
		}
	})

	t.Run("credential message exactly at the limit is accepted", func(t *testing.T) {
		credentials := []byte("\x00user\x00" + password)
		server := sasl.NewBudgetServer(context.Background(), sasl.ExchangeBudget{
			MaxSteps:        2,
			MaxMessageBytes: len(credentials),
			MaxInputBytes:   len(credentials),
		}, sasl.NewPlainServer(authenticator))

		if _, _, err := server.Next(nil); err != nil {
			t.Fatal(err)
		}
		_, done, err := server.Next(credentials)
		if err != nil || !done || !authenticated {
			t.Fatalf("expected successful auth, done=%v authenticated=%v err=%v", done, authenticated, err)
		}
	})
}
