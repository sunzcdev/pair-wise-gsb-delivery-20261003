package sasl_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
)

// endlessClient is a fake mechanism that deliberately never completes: it
// always responds and never reports an error.
type endlessClient struct {
	starts int
	nexts  int
}

func (c *endlessClient) Start() (mech string, ir []byte, err error) {
	c.starts++
	return "ENDLESS", []byte("ping"), nil
}

func (c *endlessClient) Next(challenge []byte) (response []byte, err error) {
	c.nexts++
	return []byte("ping"), nil
}

// endlessServer is a fake mechanism that deliberately never completes: it
// always sends a challenge back and never reports done.
type endlessServer struct {
	nexts int
}

func (s *endlessServer) Next(response []byte) (challenge []byte, done bool, err error) {
	s.nexts++
	return []byte("pong"), false, nil
}

func TestBudgetClientNeverEnding(t *testing.T) {
	inner := &endlessClient{}
	c := sasl.NewBudgetClient(inner, sasl.Budget{MaxSteps: 3}, nil)

	if _, _, err := c.Start(); err != nil {
		t.Fatal("Unexpected error on Start:", err)
	}
	if _, err := c.Next([]byte("pong")); err != nil {
		t.Fatal("Unexpected error on first Next:", err)
	}
	if _, err := c.Next([]byte("pong")); err != nil {
		t.Fatal("Unexpected error on second Next:", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := c.Next([]byte("pong")); err != sasl.ErrBudgetSteps {
			t.Fatal("Expected ErrBudgetSteps, got:", err)
		}
	}
	if inner.nexts != 2 {
		t.Fatal("Underlying client called after budget exhaustion, nexts =", inner.nexts)
	}
}

func TestBudgetServerNeverEnding(t *testing.T) {
	inner := &endlessServer{}
	s := sasl.NewBudgetServer(inner, sasl.Budget{MaxSteps: 2}, nil)

	if _, _, err := s.Next(nil); err != nil {
		t.Fatal("Unexpected error on first Next:", err)
	}
	if _, done, err := s.Next([]byte("ping")); err != nil || done {
		t.Fatal("Unexpected error/done on second Next:", err, done)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := s.Next([]byte("ping")); err != sasl.ErrBudgetSteps {
			t.Fatal("Expected ErrBudgetSteps, got:", err)
		}
	}
	if inner.nexts != 2 {
		t.Fatal("Underlying server called after budget exhaustion, nexts =", inner.nexts)
	}
}

func TestBudgetLoginClientBoundaries(t *testing.T) {
	challenge := []byte("Password:")

	t.Run("per-message limit exact fit", func(t *testing.T) {
		c := sasl.NewBudgetClient(sasl.NewLoginClient("username", "password"),
			sasl.Budget{MaxMessageBytes: len(challenge)}, nil)
		if _, _, err := c.Start(); err != nil {
			t.Fatal("Unexpected error on Start:", err)
		}
		resp, err := c.Next(challenge)
		if err != nil {
			t.Fatal("Unexpected error on Next:", err)
		}
		if !bytes.Equal(resp, []byte("password")) {
			t.Fatal("Invalid response:", resp)
		}
	})

	t.Run("per-message limit exceeded by one", func(t *testing.T) {
		c := sasl.NewBudgetClient(sasl.NewLoginClient("username", "password"),
			sasl.Budget{MaxMessageBytes: len(challenge) - 1}, nil)
		if _, _, err := c.Start(); err != nil {
			t.Fatal("Unexpected error on Start:", err)
		}
		for i := 0; i < 2; i++ {
			if _, err := c.Next(challenge); err != sasl.ErrBudgetMessage {
				t.Fatal("Expected ErrBudgetMessage, got:", err)
			}
		}
	})

	t.Run("step budget blocks password step", func(t *testing.T) {
		c := sasl.NewBudgetClient(sasl.NewLoginClient("username", "password"),
			sasl.Budget{MaxSteps: 1}, nil)
		if _, _, err := c.Start(); err != nil {
			t.Fatal("Unexpected error on Start:", err)
		}
		if _, err := c.Next(challenge); err != sasl.ErrBudgetSteps {
			t.Fatal("Expected ErrBudgetSteps, got:", err)
		}
	})

	t.Run("cumulative input budget", func(t *testing.T) {
		c := sasl.NewBudgetClient(sasl.NewLoginClient("username", "password"),
			sasl.Budget{MaxInputBytes: len(challenge) - 1}, nil)
		if _, _, err := c.Start(); err != nil {
			t.Fatal("Unexpected error on Start:", err)
		}
		if _, err := c.Next(challenge); err != sasl.ErrBudgetInput {
			t.Fatal("Expected ErrBudgetInput, got:", err)
		}
	})

	t.Run("output truncation delivers and reports", func(t *testing.T) {
		c := sasl.NewBudgetClient(sasl.NewLoginClient("username", "password"),
			sasl.Budget{MaxOutputBytes: len("username") + 3}, nil)
		if _, ir, err := c.Start(); err != nil {
			t.Fatal("Unexpected error on Start:", err)
		} else if !bytes.Equal(ir, []byte("username")) {
			t.Fatal("Invalid initial response:", ir)
		}
		resp, err := c.Next(challenge)
		if err != sasl.ErrBudgetOutput {
			t.Fatal("Expected ErrBudgetOutput, got:", err)
		}
		if !bytes.Equal(resp, []byte("pas")) {
			t.Fatal("Expected truncated response, got:", resp)
		}
		if strings.Contains(err.Error(), "password") {
			t.Fatal("Budget error leaks password:", err)
		}
		// Terminal state is stable and sticky.
		if _, err := c.Next(challenge); err != sasl.ErrBudgetOutput {
			t.Fatal("Expected sticky ErrBudgetOutput, got:", err)
		}
	})
}

func TestBudgetOAuthBearerServerBoundaries(t *testing.T) {
	token := "VkIvciKi9ijpiKNWrQmYCJrzgd9QYCMB"
	newServer := func() sasl.Server {
		return sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if opts.Token == token {
				return nil
			}
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		})
	}
	ir := []byte("n,a=user,\x01auth=Bearer " + token + "\x01\x01")

	t.Run("full exchange within budget", func(t *testing.T) {
		s := sasl.NewBudgetServer(newServer(), sasl.Budget{MaxSteps: 2}, nil)
		challenge, done, err := s.Next(nil)
		if err != nil || done {
			t.Fatal("Unexpected error/done on first Next:", err, done)
		}
		if challenge == nil || len(challenge) != 0 {
			t.Fatal("Expected non-nil empty challenge, got:", challenge)
		}
		if _, done, err := s.Next(ir); err != nil || !done {
			t.Fatal("Unexpected error/done on second Next:", err, done)
		}
	})

	t.Run("step budget blocks credentials step", func(t *testing.T) {
		s := sasl.NewBudgetServer(newServer(), sasl.Budget{MaxSteps: 1}, nil)
		if _, _, err := s.Next(nil); err != nil {
			t.Fatal("Unexpected error on first Next:", err)
		}
		for i := 0; i < 2; i++ {
			if _, _, err := s.Next(ir); err != sasl.ErrBudgetSteps {
				t.Fatal("Expected ErrBudgetSteps, got:", err)
			}
		}
	})

	t.Run("per-message limit exact fit", func(t *testing.T) {
		s := sasl.NewBudgetServer(newServer(), sasl.Budget{MaxMessageBytes: len(ir)}, nil)
		if _, _, err := s.Next(nil); err != nil {
			t.Fatal("Unexpected error on first Next:", err)
		}
		if _, done, err := s.Next(ir); err != nil || !done {
			t.Fatal("Unexpected error/done on second Next:", err, done)
		}
	})

	t.Run("per-message limit exceeded by one", func(t *testing.T) {
		s := sasl.NewBudgetServer(newServer(), sasl.Budget{MaxMessageBytes: len(ir) - 1}, nil)
		if _, _, err := s.Next(nil); err != nil {
			t.Fatal("Unexpected error on first Next:", err)
		}
		if _, _, err := s.Next(ir); err != sasl.ErrBudgetMessage {
			t.Fatal("Expected ErrBudgetMessage, got:", err)
		}
	})

	t.Run("error challenge truncated without leaking token", func(t *testing.T) {
		s := sasl.NewBudgetServer(newServer(), sasl.Budget{MaxOutputBytes: 5}, nil)
		if _, _, err := s.Next(nil); err != nil {
			t.Fatal("Unexpected error on first Next:", err)
		}
		badIR := []byte("n,a=user,\x01auth=Bearer wrongtoken\x01\x01")
		challenge, done, err := s.Next(badIR)
		if err != sasl.ErrBudgetOutput {
			t.Fatal("Expected ErrBudgetOutput, got:", err)
		}
		if done {
			t.Fatal("Exchange unexpectedly done")
		}
		if len(challenge) != 5 {
			t.Fatal("Expected truncated challenge of 5 bytes, got:", len(challenge))
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "wrongtoken") {
			t.Fatal("Budget error leaks token:", err)
		}
		// Terminal state is stable and sticky.
		if _, _, err := s.Next([]byte{0x01}); err != sasl.ErrBudgetOutput {
			t.Fatal("Expected sticky ErrBudgetOutput, got:", err)
		}
	})
}

func TestBudgetCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	inner := &endlessClient{}
	c := sasl.NewBudgetClient(inner, sasl.Budget{}, ctx)
	if _, _, err := c.Start(); err != nil {
		t.Fatal("Unexpected error on Start:", err)
	}

	cancel()

	for i := 0; i < 3; i++ {
		if _, err := c.Next([]byte("pong")); err != context.Canceled {
			t.Fatal("Expected context.Canceled, got:", err)
		}
	}
	if inner.nexts != 0 {
		t.Fatal("Underlying client called after cancellation, nexts =", inner.nexts)
	}

	innerServer := &endlessServer{}
	s := sasl.NewBudgetServer(innerServer, sasl.Budget{}, ctx)
	if _, _, err := s.Next(nil); err != context.Canceled {
		t.Fatal("Expected context.Canceled, got:", err)
	}
	if innerServer.nexts != 0 {
		t.Fatal("Underlying server called after cancellation, nexts =", innerServer.nexts)
	}
}

func TestBudgetPreservesNilVsEmpty(t *testing.T) {
	t.Run("nil initial response", func(t *testing.T) {
		c := sasl.NewBudgetClient(&nilIRClient{}, sasl.Budget{}, nil)
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal("Unexpected error:", err)
		}
		if ir != nil {
			t.Fatal("Expected nil initial response, got:", ir)
		}
	})

	t.Run("empty initial response", func(t *testing.T) {
		c := sasl.NewBudgetClient(&emptyIRClient{}, sasl.Budget{}, nil)
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal("Unexpected error:", err)
		}
		if ir == nil || len(ir) != 0 {
			t.Fatal("Expected non-nil empty initial response, got:", ir)
		}
	})

	t.Run("truncation to zero keeps non-nil", func(t *testing.T) {
		// The initial response consumes the whole output budget, so the
		// next response is truncated to zero bytes.
		c := sasl.NewBudgetClient(&endlessClient{}, sasl.Budget{MaxOutputBytes: len("ping")}, nil)
		_, ir, err := c.Start()
		if err != nil {
			t.Fatal("Unexpected error:", err)
		}
		if !bytes.Equal(ir, []byte("ping")) {
			t.Fatal("Invalid initial response:", ir)
		}
		resp, err := c.Next([]byte("x"))
		if err != sasl.ErrBudgetOutput {
			t.Fatal("Expected ErrBudgetOutput, got:", err)
		}
		if resp == nil || len(resp) != 0 {
			t.Fatal("Expected non-nil empty truncated response, got:", resp)
		}
	})
}

type nilIRClient struct{}

func (c *nilIRClient) Start() (mech string, ir []byte, err error) {
	return "NILIR", nil, nil
}

func (c *nilIRClient) Next(challenge []byte) (response []byte, err error) {
	return nil, nil
}

type emptyIRClient struct{}

func (c *emptyIRClient) Start() (mech string, ir []byte, err error) {
	return "EMPTYIR", []byte{}, nil
}

func (c *emptyIRClient) Next(challenge []byte) (response []byte, err error) {
	return []byte("data"), nil
}
