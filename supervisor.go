// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// ErrSupervisorStopped is returned by Supervisor.Client when the supervisor
// has permanently stopped, either because Supervisor.Kill was called or
// because the restart policy gave up after exhausting MaxRestarts or Budget.
// The returned error wraps the cause, so errors.Is(err, ErrSupervisorStopped)
// reports true for the terminal error exposed via Supervisor.Err.
var ErrSupervisorStopped = errors.New("plugin supervisor stopped")

// SupervisorState describes where the supervisor is in its lifecycle.
type SupervisorState int

const (
	// SupervisorStateStarting means the supervisor is attempting to start a
	// new plugin generation, but no generation is ready yet.
	SupervisorStateStarting SupervisorState = iota

	// SupervisorStateRunning means a plugin generation is up and ready to
	// serve clients.
	SupervisorStateRunning

	// SupervisorStateBackoff means the last generation failed and the
	// supervisor is waiting out a backoff delay before restarting.
	SupervisorStateBackoff

	// SupervisorStateStopped means the supervisor has permanently stopped
	// and will not start any further generations.
	SupervisorStateStopped
)

func (s SupervisorState) String() string {
	switch s {
	case SupervisorStateStarting:
		return "starting"
	case SupervisorStateRunning:
		return "running"
	case SupervisorStateBackoff:
		return "backoff"
	case SupervisorStateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// SupervisorConfig configures the restart policy of a Supervisor. The zero
// value is valid and enables restarts with a capped exponential backoff and
// no limit on the number of restarts.
type SupervisorConfig struct {
	// InitialBackoff is the delay before the first restart attempt. The
	// delay grows exponentially after each consecutive failure. Defaults to
	// 500ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the backoff delay between restart attempts.
	// Defaults to 30s.
	MaxBackoff time.Duration

	// Multiplier is the factor by which the backoff grows after each
	// consecutive failure. Defaults to 2.
	Multiplier float64

	// MaxRestarts is the maximum number of restarts allowed after a run of
	// consecutive failures. Once exceeded, the supervisor stops permanently.
	// Zero means no limit.
	MaxRestarts int

	// Budget is the total wall-clock time the supervisor will spend
	// recovering from a run of consecutive failures, measured from the first
	// failure. Once exceeded, the supervisor stops permanently. Zero means
	// no limit.
	Budget time.Duration

	// StableAfter, if non-zero, marks a generation that runs longer than
	// this duration as healthy: the restart counter, backoff, and budget
	// window are all reset when such a generation eventually exits. Zero
	// means failures always accumulate.
	StableAfter time.Duration

	// Logger is the logger used by the supervisor itself. Plugin output
	// continues to flow through the logger configured on each Client. If
	// nil, a default logger is used.
	Logger hclog.Logger
}

// Supervisor keeps a plugin running across crashes. It is optional
// infrastructure for long-running plugins: create one with NewSupervisor,
// then use Client to obtain the current generation's protocol client.
//
// Every (re)start of the plugin is an independent generation built from a
// fresh Client produced by the factory function. Generations never share
// processes, addresses, connections, or brokers, so a Client, a dispensed
// plugin implementation, or a broker connection obtained from an older
// generation can never silently attach to a newer process: they keep
// talking to the dead process and fail instead. For a cryptographic
// guarantee that an old connection cannot be accepted by a new process,
// enable AutoMTLS on the ClientConfig, which issues per-generation
// certificates.
//
// Calls that are in flight when a generation crashes fail with
// transport-level errors that are distinguishable from application errors
// returned by the plugin itself: gRPC calls fail with
// codes.Unavailable (or codes.Canceled once the dead generation's
// connection has been closed), net/rpc calls fail with io.EOF or a
// connection reset. Whether to retry is a business decision; use
// Generation to detect that a restart happened, then re-acquire a client
// via Client and re-dispense before retrying.
type Supervisor struct {
	newClient func() (*Client, error)
	config    SupervisorConfig
	logger    hclog.Logger

	mu         sync.Mutex
	started    bool
	killed     bool
	stopped    bool
	state      SupervisorState
	err        error
	current    *Client
	generation uint64
	restarts   int
	readyCh    chan struct{}
	killCh     chan struct{}
	doneCh     chan struct{}
	loopDoneCh chan struct{}
}

// NewSupervisor creates a supervisor that keeps a plugin alive according to
// the given restart policy. newClient is called once per generation and must
// return a new, unstarted Client each time; in particular a ClientConfig.Cmd
// cannot be reused across generations because exec.Cmd cannot be restarted.
//
// The returned supervisor does not start the plugin until Start or Client is
// called.
func NewSupervisor(newClient func() (*Client, error), config SupervisorConfig) *Supervisor {
	if config.InitialBackoff <= 0 {
		config.InitialBackoff = 500 * time.Millisecond
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 30 * time.Second
	}
	if config.Multiplier <= 0 {
		config.Multiplier = 2
	}
	if config.Logger == nil {
		config.Logger = hclog.New(&hclog.LoggerOptions{
			Output: hclog.DefaultOutput,
			Level:  hclog.Trace,
			Name:   "plugin.supervisor",
		})
	}

	return &Supervisor{
		newClient:  newClient,
		config:     config,
		logger:     config.Logger,
		state:      SupervisorStateStarting,
		readyCh:    make(chan struct{}),
		killCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		loopDoneCh: make(chan struct{}),
	}
}

// Start launches the supervisor loop and the first plugin generation. It is
// idempotent and returns immediately; use Client to wait for a usable
// generation.
func (s *Supervisor) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return
	}
	s.started = true

	if s.killed {
		// Kill was called before Start; never run anything.
		s.stopped = true
		close(s.doneCh)
		close(s.loopDoneCh)
		return
	}

	go s.run()
}

// Client returns the protocol client of the current plugin generation,
// waiting while a generation is (re)starting. It returns an error matching
// ErrSupervisorStopped if the supervisor has permanently stopped, or the
// context error if ctx is canceled while waiting.
//
// The returned client is bound to its generation. After a restart it must
// not be used again; call Client once more to get the new generation.
func (s *Supervisor) Client(ctx context.Context) (ClientProtocol, error) {
	s.Start()

	for {
		s.mu.Lock()
		current := s.current
		stopped := s.stopped
		err := s.err
		readyCh := s.readyCh
		s.mu.Unlock()

		if current != nil && !current.Exited() {
			return current.Client()
		}
		if stopped {
			if err != nil {
				return nil, err
			}
			return nil, ErrSupervisorStopped
		}

		select {
		case <-readyCh:
		case <-s.doneCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Generation returns the current generation number. It is incremented every
// time a new plugin process successfully starts and handshakes. A change in
// generation means any previously obtained Client, dispensed implementation,
// or broker connection belongs to a dead process.
func (s *Supervisor) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// State returns the current supervisor state.
func (s *Supervisor) State() SupervisorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Err returns the terminal error that caused the supervisor to give up, or
// nil if it is still running or was stopped by Kill. The error matches
// ErrSupervisorStopped via errors.Is.
func (s *Supervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Done returns a channel that is closed once the supervisor has permanently
// stopped, either by giving up or via Kill.
func (s *Supervisor) Done() <-chan struct{} {
	return s.doneCh
}

// Kill stops the supervisor and gracefully kills the current plugin
// generation, if any. A killed supervisor never restarts the plugin, even if
// Kill is called while the supervisor is waiting out a restart backoff. It
// blocks until the supervisor loop has finished and is safe to call multiple
// times.
func (s *Supervisor) Kill() {
	s.mu.Lock()
	if s.killed {
		s.mu.Unlock()
		<-s.loopDoneCh
		return
	}
	s.killed = true
	close(s.killCh)
	started := s.started
	s.mu.Unlock()

	if !started {
		// Start has not been called yet; make sure a later Start is a no-op
		// and that Done/loop waiters are released.
		s.Start()
	}
	<-s.loopDoneCh
}

// setState records a new supervisor state.
func (s *Supervisor) setState(state SupervisorState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.state = state
	}
}

// stop permanently stops the supervisor, recording err as the terminal
// error (nil when stopped by the user via Kill).
func (s *Supervisor) stop(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	s.state = SupervisorStateStopped
	s.err = err
	close(s.doneCh)
}

// isKilled reports whether Kill was requested.
func (s *Supervisor) isKilled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killed
}

// run is the supervisor loop. It (re)starts generations, watches them, and
// applies the restart policy until it gives up or is killed.
func (s *Supervisor) run() {
	defer close(s.loopDoneCh)

	backoff := s.config.InitialBackoff
	restarts := 0
	var budgetStart time.Time

	for {
		if s.isKilled() {
			s.stop(nil)
			return
		}

		s.setState(SupervisorStateStarting)
		client, err := s.newClient()
		if err == nil {
			_, err = client.Start()
		}
		if err != nil {
			s.logger.Warn("plugin generation failed to start", "error", err)
			if client != nil {
				client.Kill()
			}
		} else {
			s.mu.Lock()
			s.generation++
			s.current = client
			s.state = SupervisorStateRunning
			generation := s.generation
			close(s.readyCh)
			s.readyCh = make(chan struct{})
			s.mu.Unlock()

			startedAt := time.Now()

			// Wait for the process to exit, or for the user to kill the
			// supervisor.
			select {
			case <-client.done():
			case <-s.killCh:
			}

			s.mu.Lock()
			s.current = nil
			killed := s.killed
			s.mu.Unlock()

			// Reap the dead (or being-killed) generation. This is a no-op
			// for an already exited process and never triggers a restart.
			client.Kill()

			if killed {
				s.stop(nil)
				return
			}

			s.logger.Warn("plugin generation exited", "generation", generation)

			if s.config.StableAfter > 0 && time.Since(startedAt) >= s.config.StableAfter {
				// The generation was healthy for a good while; treat the
				// next failure as a fresh run.
				restarts = 0
				backoff = s.config.InitialBackoff
				budgetStart = time.Time{}
			}
		}

		// Failure path: decide whether to restart or give up.
		if s.config.MaxRestarts > 0 && restarts >= s.config.MaxRestarts {
			s.stop(fmt.Errorf("%w: gave up after %d restarts", ErrSupervisorStopped, restarts))
			return
		}
		if budgetStart.IsZero() {
			budgetStart = time.Now()
		}
		if s.config.Budget > 0 && time.Since(budgetStart) >= s.config.Budget {
			s.stop(fmt.Errorf("%w: restart budget of %s exhausted", ErrSupervisorStopped, s.config.Budget))
			return
		}

		restarts++
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()

		s.logger.Warn("restarting plugin after backoff",
			"backoff", backoff, "restart", restarts)
		s.setState(SupervisorStateBackoff)

		timer := time.NewTimer(backoff)
		select {
		case <-s.killCh:
			timer.Stop()
			s.stop(nil)
			return
		case <-timer.C:
		}

		backoff = time.Duration(float64(backoff) * s.config.Multiplier)
		if backoff > s.config.MaxBackoff {
			backoff = s.config.MaxBackoff
		}
	}
}
