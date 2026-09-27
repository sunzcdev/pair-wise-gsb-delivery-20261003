package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

// controlledSchedule lets a test decide exactly when triggers happen: every
// value sent on trigger is returned by the next Next call. entered signals
// that the scheduler has called Next, i.e. it has finished dispatching the
// previous trigger.
type controlledSchedule struct {
	trigger chan time.Time
	entered chan struct{}
}

func newControlledSchedule() *controlledSchedule {
	return &controlledSchedule{
		trigger: make(chan time.Time),
		entered: make(chan struct{}, 100),
	}
}

func (s *controlledSchedule) Next(time.Time) time.Time {
	s.entered <- struct{}{}
	return <-s.trigger
}

// fire delivers a trigger time and waits until the scheduler has dispatched
// it and asked for the next activation.
func (s *controlledSchedule) fire(t time.Time) {
	s.trigger <- t
	<-s.entered
}

// unblock releases a blocked Next call with a far-future activation so the
// scheduler returns to its select loop and can process Stop or Remove.
func (s *controlledSchedule) unblock() {
	s.trigger <- time.Now().Add(time.Hour)
}

// blockingJob records the trigger time of every run and blocks until the
// test releases it.
type blockingJob struct {
	mu      chan struct{}
	times   []time.Time
	started chan struct{}
	release chan struct{}
}

func newBlockingJob() *blockingJob {
	return &blockingJob{
		mu:      make(chan struct{}, 1),
		started: make(chan struct{}, 100),
		release: make(chan struct{}),
	}
}

func (j *blockingJob) RunAt(t time.Time) {
	j.mu <- struct{}{}
	j.times = append(j.times, t)
	<-j.mu
	j.started <- struct{}{}
	<-j.release
}

// Run is only a fallback; the scheduler prefers RunAt since blockingJob
// implements TriggerJob.
func (j *blockingJob) Run() { j.RunAt(time.Now()) }

func (j *blockingJob) runCount() int {
	j.mu <- struct{}{}
	defer func() { <-j.mu }()
	return len(j.times)
}

func (j *blockingJob) runTime(i int) time.Time {
	j.mu <- struct{}{}
	defer func() { <-j.mu }()
	return j.times[i]
}

// panickingJob panics at the end of its first run. It implements only Run,
// exercising the fallback for jobs that do not implement TriggerJob.
type panickingJob struct {
	mu      chan struct{}
	runs    int
	started chan struct{}
	release chan struct{}
}

func newPanickingJob() *panickingJob {
	return &panickingJob{
		mu:      make(chan struct{}, 1),
		started: make(chan struct{}, 100),
		release: make(chan struct{}),
	}
}

func (j *panickingJob) Run() {
	j.mu <- struct{}{}
	j.runs++
	run := j.runs
	<-j.mu
	j.started <- struct{}{}
	<-j.release
	if run == 1 {
		panic("boom")
	}
}

func (j *panickingJob) runCount() int {
	j.mu <- struct{}{}
	defer func() { <-j.mu }()
	return j.runs
}

func waitRunStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(OneSecond):
		t.Fatal("timed out waiting for job run to start")
	}
}

func expectNoRun(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
		t.Fatal("unexpected extra job run")
	case <-time.After(200 * time.Millisecond):
	}
}

func waitStopped(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(OneSecond):
		t.Fatal("timed out waiting for cron to stop")
	}
}

// Ten triggers fired while the job is blocked must produce exactly two runs:
// the one in progress plus a single coalesced run stamped with the most
// recent trigger time. After Stop, no further run may start.
func TestCoalesceIfStillRunning(t *testing.T) {
	sched := newControlledSchedule()
	job := newBlockingJob()
	cron := New(WithChain(CoalesceIfStillRunning(DiscardLogger)))
	cron.Schedule(sched, job)
	cron.Start()
	<-sched.entered // initial Next call

	base := time.Now().Add(-time.Hour)
	firstTrigger := base.Add(time.Second)
	sched.fire(firstTrigger)
	waitRunStarted(t, job.started) // first run is now blocked

	// Fire 9 more triggers while the first run is still in progress.
	var lastTrigger time.Time
	for i := 2; i <= 10; i++ {
		lastTrigger = base.Add(time.Duration(i) * time.Second)
		sched.fire(lastTrigger)
	}

	// Give the dispatched triggers a moment to reach the job wrapper.
	time.Sleep(100 * time.Millisecond)
	if got := job.runCount(); got != 1 {
		t.Fatalf("expected 1 run while blocked, got %d", got)
	}

	// Finishing the first run immediately runs the coalesced pending run,
	// stamped with the most recent trigger time.
	job.release <- struct{}{}
	waitRunStarted(t, job.started)
	if got := job.runCount(); got != 2 {
		t.Fatalf("expected 2 runs after 10 triggers, got %d", got)
	}
	if got := job.runTime(0); !got.Equal(firstTrigger) {
		t.Errorf("expected first run to use trigger time %v, got %v", firstTrigger, got)
	}
	if got := job.runTime(1); !got.Equal(lastTrigger) {
		t.Errorf("expected coalesced run to use last trigger time %v, got %v", lastTrigger, got)
	}
	job.release <- struct{}{}

	expectNoRun(t, job.started)

	// Stop the scheduler; no further run may start afterwards.
	sched.unblock()
	ctx := cron.Stop()
	waitStopped(t, ctx)
	expectNoRun(t, job.started)
	if got := job.runCount(); got != 2 {
		t.Fatalf("expected 2 runs after stop, got %d", got)
	}
}

// A coalesced pending run belongs to triggers already delivered to the job,
// so it still executes when Stop arrives while the current run is blocked;
// Stop's context completes only afterwards, and nothing runs after that.
func TestCoalesceStopWithPendingRun(t *testing.T) {
	sched := newControlledSchedule()
	job := newBlockingJob()
	cron := New(WithChain(CoalesceIfStillRunning(DiscardLogger)))
	cron.Schedule(sched, job)
	cron.Start()
	<-sched.entered

	base := time.Now().Add(-time.Hour)
	sched.fire(base.Add(time.Second))
	waitRunStarted(t, job.started)

	var lastTrigger time.Time
	for i := 2; i <= 5; i++ {
		lastTrigger = base.Add(time.Duration(i) * time.Second)
		sched.fire(lastTrigger)
	}
	time.Sleep(100 * time.Millisecond)

	// Stop the scheduler while a run is blocked and a coalesced run pending.
	sched.unblock()
	ctx := cron.Stop()

	// The pending run still executes, stamped with the last trigger time.
	job.release <- struct{}{}
	waitRunStarted(t, job.started)
	if got := job.runTime(1); !got.Equal(lastTrigger) {
		t.Errorf("expected pending run to use last trigger time %v, got %v", lastTrigger, got)
	}
	job.release <- struct{}{}

	waitStopped(t, ctx)
	expectNoRun(t, job.started)
	if got := job.runCount(); got != 2 {
		t.Fatalf("expected 2 runs total, got %d", got)
	}
}

// Removing an entry does not cancel an already-coalesced pending run, but no
// new triggers are delivered afterwards.
func TestCoalesceRemoveWithPendingRun(t *testing.T) {
	sched := newControlledSchedule()
	job := newBlockingJob()
	cron := New(WithChain(CoalesceIfStillRunning(DiscardLogger)))
	id := cron.Schedule(sched, job)
	cron.Start()
	<-sched.entered

	base := time.Now().Add(-time.Hour)
	sched.fire(base.Add(time.Second))
	waitRunStarted(t, job.started)
	lastTrigger := base.Add(2 * time.Second)
	sched.fire(lastTrigger)
	time.Sleep(100 * time.Millisecond)

	sched.unblock()
	cron.Remove(id)

	job.release <- struct{}{}
	waitRunStarted(t, job.started)
	if got := job.runTime(1); !got.Equal(lastTrigger) {
		t.Errorf("expected pending run to use last trigger time %v, got %v", lastTrigger, got)
	}
	job.release <- struct{}{}

	expectNoRun(t, job.started)
	ctx := cron.Stop()
	waitStopped(t, ctx)
	if got := job.runCount(); got != 2 {
		t.Fatalf("expected 2 runs total, got %d", got)
	}
}

// A panic in a run is recovered so the pending run still executes and the
// job is not left permanently "running"; the panic is re-raised so an outer
// Recover wrapper logs it, and later triggers run normally.
func TestCoalesceRecoversPanic(t *testing.T) {
	var buf syncWriter
	sched := newControlledSchedule()
	job := newPanickingJob()
	cron := New(WithChain(
		Recover(newBufLogger(&buf)),
		CoalesceIfStillRunning(DiscardLogger),
	))
	cron.Schedule(sched, job)
	cron.Start()
	<-sched.entered

	base := time.Now().Add(-time.Hour)
	sched.fire(base.Add(time.Second))
	waitRunStarted(t, job.started)
	sched.fire(base.Add(2 * time.Second))
	time.Sleep(100 * time.Millisecond)

	// The first run panics; the pending run must still execute.
	job.release <- struct{}{}
	waitRunStarted(t, job.started)
	job.release <- struct{}{}

	// The panic is re-raised and logged by the Recover wrapper.
	deadline := time.After(OneSecond)
	for !strings.Contains(buf.String(), "boom") {
		select {
		case <-deadline:
			t.Fatal("expected panic to be logged")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// The job is not stuck: a later trigger runs normally.
	sched.fire(base.Add(3 * time.Second))
	waitRunStarted(t, job.started)
	job.release <- struct{}{}

	sched.unblock()
	ctx := cron.Stop()
	waitStopped(t, ctx)
	if got := job.runCount(); got != 3 {
		t.Fatalf("expected 3 runs, got %d", got)
	}
}
