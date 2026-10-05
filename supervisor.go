// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

var (
	// ErrPluginCrashed is returned (or wrapped) when a supervised plugin
	// process exited without being asked to. Calls to Client or Start while
	// the supervisor is waiting to restart the plugin return an error
	// wrapping ErrPluginCrashed, and the generation context passed to
	// GRPCPlugin.GRPCClient is canceled with ErrPluginCrashed as its cause.
	ErrPluginCrashed = errors.New("plugin process exited unexpectedly")

	// ErrPluginStaleGeneration is returned when using a ClientProtocol that
	// was obtained from a previous plugin process generation. After the
	// supervisor restarts a plugin, old clients, dispensed implementations
	// and broker connections are never silently attached to the new
	// process; new instances must be obtained via Client and Dispense.
	ErrPluginStaleGeneration = errors.New("plugin client belongs to a previous plugin process generation")

	// ErrPluginSupervisorGaveUp is returned once the supervisor has
	// exhausted its restart count or time budget and will no longer
	// restart the plugin.
	ErrPluginSupervisorGaveUp = errors.New("plugin supervisor gave up restarting the plugin")

	// ErrPluginKilled is returned by Start and Client on a supervised
	// client after Kill has been called. A user-initiated Kill never
	// triggers a restart.
	ErrPluginKilled = errors.New("plugin has been killed")

	// ErrSupervisorAndReattach is returned when both Supervisor and
	// Reattach are set. Supervision requires managing the plugin process
	// lifecycle itself.
	ErrSupervisorAndReattach = errors.New("only one of Supervisor or Reattach can be set")
)

// SupervisorConfig configures the optional plugin process supervisor. When
// set on a ClientConfig, the client restarts the plugin process with
// exponential backoff if it exits without being killed by the user, and
// gives up once MaxRestarts or Budget is exceeded.
//
// Each (re)start of the plugin is an independent generation: the previous
// generation's ClientProtocol, dispensed implementations and broker
// connections are never reused for the new process. In-flight calls against
// a crashed generation fail; whether to retry them is a decision left to
// the caller, which can obtain a client for the new generation by calling
// Client again.
type SupervisorConfig struct {
	// MaxRestarts is the maximum number of times the supervisor will
	// attempt to restart the plugin over the client's lifetime. Once
	// exceeded, the supervisor gives up. Zero means no limit.
	MaxRestarts int

	// InitialBackoff is how long the supervisor waits before the first
	// restart attempt after a crash. The wait doubles after each
	// consecutive failed attempt, up to MaxBackoff, and resets once a new
	// generation starts successfully. Defaults to 500ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the wait between restart attempts.
	// Defaults to 30s.
	MaxBackoff time.Duration

	// Budget is the total amount of time, counted from the first crash,
	// during which the supervisor will attempt restarts. Once exceeded,
	// the supervisor gives up. Zero means no time limit.
	Budget time.Duration

	// Notify, if set, is called whenever the supervisor schedules a
	// restart, successfully restarts the plugin, or gives up. It is called
	// from the supervisor goroutine and must not block.
	Notify func(SupervisorEvent)
}

// SupervisorEventKind identifies the type of a SupervisorEvent.
type SupervisorEventKind int

const (
	// SupervisorEventRestarting indicates the supervisor is waiting out a
	// backoff before attempting to restart the plugin.
	SupervisorEventRestarting SupervisorEventKind = iota

	// SupervisorEventRestarted indicates a new plugin process generation
	// started successfully.
	SupervisorEventRestarted

	// SupervisorEventGaveUp indicates the supervisor exhausted its restart
	// count or time budget and will no longer restart the plugin.
	SupervisorEventGaveUp
)

// SupervisorEvent describes a supervisor state transition. See
// SupervisorConfig.Notify.
type SupervisorEvent struct {
	// Kind is the type of transition.
	Kind SupervisorEventKind

	// Generation is the process generation the event relates to: the
	// crashed generation for SupervisorEventRestarting and
	// SupervisorEventGaveUp, or the new generation for
	// SupervisorEventRestarted.
	Generation uint64

	// Restarts is the number of restart attempts made so far.
	Restarts int

	// Backoff is the wait before the next restart attempt. It is only set
	// for SupervisorEventRestarting.
	Backoff time.Duration

	// Err is the error that caused the crash or failed restart attempt,
	// if any.
	Err error
}

// crashEvent is sent to the supervisor loop when a plugin process
// generation exits unexpectedly or fails to start.
type crashEvent struct {
	gen uint64
	err error
}

// clientSupervisor restarts a supervised Client's plugin process after it
// exits unexpectedly. A single goroutine runs the state machine so that
// restarts are serialized with each other and with Kill.
type clientSupervisor struct {
	config SupervisorConfig

	// exitCh carries crash events to the supervisor loop. It is buffered
	// so that senders never block; stale events are dropped by the loop.
	exitCh chan crashEvent

	// stopCh is closed by Kill to stop the supervisor promptly, even
	// while it is waiting out a backoff.
	stopCh   chan struct{}
	stopOnce sync.Once

	// doneCh is closed when the supervisor loop exits.
	doneCh chan struct{}

	// The following fields are guarded by the owning Client's mutex.
	gaveUp  bool
	lastErr error
	active  bool
}

func newClientSupervisor(config SupervisorConfig) *clientSupervisor {
	if config.InitialBackoff <= 0 {
		config.InitialBackoff = 500 * time.Millisecond
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 30 * time.Second
	}
	if config.MaxBackoff < config.InitialBackoff {
		config.MaxBackoff = config.InitialBackoff
	}
	return &clientSupervisor{
		config: config,
		exitCh: make(chan crashEvent, 1),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// notifyCrashed delivers a crash event to the supervisor loop. It never
// blocks; if an event is already pending the new one is dropped, since the
// loop re-checks the client state after every restart attempt anyway.
func (s *clientSupervisor) notifyCrashed(ev crashEvent) {
	select {
	case s.exitCh <- ev:
	default:
	}
}

// requestStop asks the supervisor loop to exit. It is idempotent.
func (s *clientSupervisor) requestStop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// waitStopped blocks until the supervisor loop has exited.
func (s *clientSupervisor) waitStopped() {
	<-s.doneCh
}

func (s *clientSupervisor) notify(ev SupervisorEvent) {
	if s.config.Notify != nil {
		s.config.Notify(ev)
	}
}

// giveUp marks the supervisor as permanently failed.
func (s *clientSupervisor) giveUp(c *Client, err error, restarts int) {
	c.l.Lock()
	s.gaveUp = true
	s.lastErr = err
	s.active = false
	gen := c.generation
	c.l.Unlock()

	s.notify(SupervisorEvent{
		Kind:       SupervisorEventGaveUp,
		Generation: gen,
		Restarts:   restarts,
		Err:        err,
	})
}

// run is the supervisor loop. It consumes crash events and restarts the
// plugin with exponential backoff until it is stopped or gives up.
func (s *clientSupervisor) run(c *Client) {
	defer close(s.doneCh)

	var restarts int
	var budgetStart time.Time

	for {
		var ev crashEvent
		select {
		case <-s.stopCh:
			return
		case ev = <-s.exitCh:
		}

		c.l.Lock()
		// Drop events from stale generations, and ignore crashes once the
		// client has been killed or the supervisor has given up.
		if ev.gen != c.generation || c.killedByUser || s.gaveUp {
			c.l.Unlock()
			continue
		}
		s.active = true
		c.l.Unlock()

		if budgetStart.IsZero() {
			budgetStart = time.Now()
		}

		backoff := s.config.InitialBackoff
		lastErr := ev.err

		for {
			if s.config.MaxRestarts > 0 && restarts >= s.config.MaxRestarts {
				s.giveUp(c, lastErr, restarts)
				break
			}
			if s.config.Budget > 0 && time.Since(budgetStart) >= s.config.Budget {
				s.giveUp(c, lastErr, restarts)
				break
			}

			s.notify(SupervisorEvent{
				Kind:       SupervisorEventRestarting,
				Generation: ev.gen,
				Restarts:   restarts,
				Backoff:    backoff,
				Err:        lastErr,
			})

			select {
			case <-s.stopCh:
				return
			case <-time.After(backoff):
			}

			c.l.Lock()
			if c.killedByUser {
				c.l.Unlock()
				return
			}

			// Start a new, fully independent generation. The old protocol
			// client is detached here and closed once the lock is
			// released; it is never reused for the new process.
			oldClient := c.client
			c.resetGenerationLocked()
			_, startErr := c.startLocked()
			if startErr == nil {
				restarts++
				s.active = false
				gen := c.generation
				c.l.Unlock()
				if oldClient != nil {
					_ = oldClient.Close()
				}
				s.notify(SupervisorEvent{
					Kind:       SupervisorEventRestarted,
					Generation: gen,
					Restarts:   restarts,
				})
				break
			}
			restarts++
			lastErr = startErr
			c.l.Unlock()
			if oldClient != nil {
				_ = oldClient.Close()
			}

			backoff *= 2
			if backoff > s.config.MaxBackoff {
				backoff = s.config.MaxBackoff
			}
		}
	}
}

// supervisedClientProtocol wraps a ClientProtocol with a generation check
// so that use of a client from a previous plugin process generation fails
// with an identifiable error instead of silently misbehaving.
type supervisedClientProtocol struct {
	inner      ClientProtocol
	client     *Client
	generation uint64
}

func (s *supervisedClientProtocol) checkGeneration() error {
	s.client.l.Lock()
	defer s.client.l.Unlock()

	switch {
	case s.client.killedByUser:
		return ErrPluginKilled
	case s.client.sup.gaveUp:
		return fmt.Errorf("%w: %w", ErrPluginSupervisorGaveUp, s.client.sup.lastErr)
	case s.client.generation != s.generation:
		return fmt.Errorf("%w: client is from generation %d, current generation is %d",
			ErrPluginStaleGeneration, s.generation, s.client.generation)
	case s.client.exited:
		return fmt.Errorf("%w: waiting for supervisor restart", ErrPluginCrashed)
	}
	return nil
}

func (s *supervisedClientProtocol) Dispense(name string) (interface{}, error) {
	if err := s.checkGeneration(); err != nil {
		return nil, err
	}
	return s.inner.Dispense(name)
}

func (s *supervisedClientProtocol) Ping() error {
	if err := s.checkGeneration(); err != nil {
		return err
	}
	return s.inner.Ping()
}

func (s *supervisedClientProtocol) Close() error {
	return s.inner.Close()
}

// cloneCmd builds a fresh, unstarted exec.Cmd from a template. Supervised
// clients need a new command for every process generation since exec.Cmd
// cannot be reused once started.
func cloneCmd(tmpl *exec.Cmd) *exec.Cmd {
	return &exec.Cmd{
		Path:        tmpl.Path,
		Args:        append([]string(nil), tmpl.Args...),
		Env:         append([]string(nil), tmpl.Env...),
		Dir:         tmpl.Dir,
		SysProcAttr: tmpl.SysProcAttr,
	}
}
