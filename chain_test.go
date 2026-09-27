package cron

import (
	"io/ioutil"
	"log"
	"reflect"
	"sync"
	"testing"
	"time"
)

func appendingJob(slice *[]int, value int) Job {
	var m sync.Mutex
	return FuncJob(func() {
		m.Lock()
		*slice = append(*slice, value)
		m.Unlock()
	})
}

func appendingWrapper(slice *[]int, value int) JobWrapper {
	return func(j Job) Job {
		return FuncJob(func() {
			appendingJob(slice, value).Run()
			j.Run()
		})
	}
}

func TestChain(t *testing.T) {
	var nums []int
	var (
		append1 = appendingWrapper(&nums, 1)
		append2 = appendingWrapper(&nums, 2)
		append3 = appendingWrapper(&nums, 3)
		append4 = appendingJob(&nums, 4)
	)
	NewChain(append1, append2, append3).Then(append4).Run()
	if !reflect.DeepEqual(nums, []int{1, 2, 3, 4}) {
		t.Error("unexpected order of calls:", nums)
	}
}

func TestChainRecover(t *testing.T) {
	panickingJob := FuncJob(func() {
		panic("panickingJob panics")
	})

	t.Run("panic exits job by default", func(t *testing.T) {
		defer func() {
			if err := recover(); err == nil {
				t.Errorf("panic expected, but none received")
			}
		}()
		NewChain().Then(panickingJob).
			Run()
	})

	t.Run("Recovering JobWrapper recovers", func(t *testing.T) {
		NewChain(Recover(PrintfLogger(log.New(ioutil.Discard, "", 0)))).
			Then(panickingJob).
			Run()
	})

	t.Run("composed with the *IfStillRunning wrappers", func(t *testing.T) {
		NewChain(Recover(PrintfLogger(log.New(ioutil.Discard, "", 0)))).
			Then(panickingJob).
			Run()
	})
}

type countJob struct {
	m       sync.Mutex
	started int
	done    int
	delay   time.Duration
}

func (j *countJob) Run() {
	j.m.Lock()
	j.started++
	j.m.Unlock()
	time.Sleep(j.delay)
	j.m.Lock()
	j.done++
	j.m.Unlock()
}

func (j *countJob) Started() int {
	defer j.m.Unlock()
	j.m.Lock()
	return j.started
}

func (j *countJob) Done() int {
	defer j.m.Unlock()
	j.m.Lock()
	return j.done
}

func TestChainDelayIfStillRunning(t *testing.T) {

	t.Run("runs immediately", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(DelayIfStillRunning(DiscardLogger)).Then(&j)
		go wrappedJob.Run()
		time.Sleep(2 * time.Millisecond) // Give the job 2ms to complete.
		if c := j.Done(); c != 1 {
			t.Errorf("expected job run once, immediately, got %d", c)
		}
	})

	t.Run("second run immediate if first done", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(DelayIfStillRunning(DiscardLogger)).Then(&j)
		go func() {
			go wrappedJob.Run()
			time.Sleep(time.Millisecond)
			go wrappedJob.Run()
		}()
		time.Sleep(3 * time.Millisecond) // Give both jobs 3ms to complete.
		if c := j.Done(); c != 2 {
			t.Errorf("expected job run twice, immediately, got %d", c)
		}
	})

	t.Run("second run delayed if first not done", func(t *testing.T) {
		var j countJob
		j.delay = 10 * time.Millisecond
		wrappedJob := NewChain(DelayIfStillRunning(DiscardLogger)).Then(&j)
		go func() {
			go wrappedJob.Run()
			time.Sleep(time.Millisecond)
			go wrappedJob.Run()
		}()

		// After 5ms, the first job is still in progress, and the second job was
		// run but should be waiting for it to finish.
		time.Sleep(5 * time.Millisecond)
		started, done := j.Started(), j.Done()
		if started != 1 || done != 0 {
			t.Error("expected first job started, but not finished, got", started, done)
		}

		// Verify that the second job completes.
		time.Sleep(25 * time.Millisecond)
		started, done = j.Started(), j.Done()
		if started != 2 || done != 2 {
			t.Error("expected both jobs done, got", started, done)
		}
	})

}

func TestChainSkipIfStillRunning(t *testing.T) {

	t.Run("runs immediately", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(SkipIfStillRunning(DiscardLogger)).Then(&j)
		go wrappedJob.Run()
		time.Sleep(2 * time.Millisecond) // Give the job 2ms to complete.
		if c := j.Done(); c != 1 {
			t.Errorf("expected job run once, immediately, got %d", c)
		}
	})

	t.Run("second run immediate if first done", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(SkipIfStillRunning(DiscardLogger)).Then(&j)
		go func() {
			go wrappedJob.Run()
			time.Sleep(time.Millisecond)
			go wrappedJob.Run()
		}()
		time.Sleep(3 * time.Millisecond) // Give both jobs 3ms to complete.
		if c := j.Done(); c != 2 {
			t.Errorf("expected job run twice, immediately, got %d", c)
		}
	})

	t.Run("second run skipped if first not done", func(t *testing.T) {
		var j countJob
		j.delay = 10 * time.Millisecond
		wrappedJob := NewChain(SkipIfStillRunning(DiscardLogger)).Then(&j)
		go func() {
			go wrappedJob.Run()
			time.Sleep(time.Millisecond)
			go wrappedJob.Run()
		}()

		// After 5ms, the first job is still in progress, and the second job was
		// aleady skipped.
		time.Sleep(5 * time.Millisecond)
		started, done := j.Started(), j.Done()
		if started != 1 || done != 0 {
			t.Error("expected first job started, but not finished, got", started, done)
		}

		// Verify that the first job completes and second does not run.
		time.Sleep(25 * time.Millisecond)
		started, done = j.Started(), j.Done()
		if started != 1 || done != 1 {
			t.Error("expected second job skipped, got", started, done)
		}
	})

	t.Run("skip 10 jobs on rapid fire", func(t *testing.T) {
		var j countJob
		j.delay = 10 * time.Millisecond
		wrappedJob := NewChain(SkipIfStillRunning(DiscardLogger)).Then(&j)
		for i := 0; i < 11; i++ {
			go wrappedJob.Run()
		}
		time.Sleep(200 * time.Millisecond)
		done := j.Done()
		if done != 1 {
			t.Error("expected 1 jobs executed, 10 jobs dropped, got", done)
		}
	})

	t.Run("different jobs independent", func(t *testing.T) {
		var j1, j2 countJob
		j1.delay = 10 * time.Millisecond
		j2.delay = 10 * time.Millisecond
		chain := NewChain(SkipIfStillRunning(DiscardLogger))
		wrappedJob1 := chain.Then(&j1)
		wrappedJob2 := chain.Then(&j2)
		for i := 0; i < 11; i++ {
			go wrappedJob1.Run()
			go wrappedJob2.Run()
		}
		time.Sleep(100 * time.Millisecond)
		var (
			done1 = j1.Done()
			done2 = j2.Done()
		)
		if done1 != 1 || done2 != 1 {
			t.Error("expected both jobs executed once, got", done1, "and", done2)
		}
	})

}

// blockingJob is a job whose runs are controlled by the test: each run
// signals its start on the started channel and blocks until a token is
// received on release. It records the trigger time of every run.
type blockingJob struct {
	m        sync.Mutex
	triggers []time.Time
	started  chan struct{}
	release  chan struct{}
}

func newBlockingJob() *blockingJob {
	return &blockingJob{
		started: make(chan struct{}, 100),
		release: make(chan struct{}, 100),
	}
}

func (j *blockingJob) Run() { j.RunWithTrigger(time.Time{}) }

func (j *blockingJob) RunWithTrigger(trigger time.Time) {
	j.m.Lock()
	j.triggers = append(j.triggers, trigger)
	j.m.Unlock()
	j.started <- struct{}{}
	<-j.release
}

func (j *blockingJob) Runs() int {
	j.m.Lock()
	defer j.m.Unlock()
	return len(j.triggers)
}

func (j *blockingJob) Trigger(i int) time.Time {
	j.m.Lock()
	defer j.m.Unlock()
	return j.triggers[i]
}

func TestChainMergeIfStillRunning(t *testing.T) {

	t.Run("runs immediately", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(&j)
		go wrappedJob.Run()
		time.Sleep(2 * time.Millisecond) // Give the job 2ms to complete.
		if c := j.Done(); c != 1 {
			t.Errorf("expected job run once, immediately, got %d", c)
		}
	})

	t.Run("second run immediate if first done", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(&j)
		go func() {
			go wrappedJob.Run()
			time.Sleep(time.Millisecond)
			go wrappedJob.Run()
		}()
		// Poll for both runs to complete, allowing for scheduling delays.
		deadline := time.After(time.Second)
		for j.Done() != 2 {
			select {
			case <-deadline:
				t.Fatalf("expected job run twice, immediately, got %d", j.Done())
			case <-time.After(time.Millisecond):
			}
		}
	})

	t.Run("ten triggers merge into one pending run", func(t *testing.T) {
		j := newBlockingJob()
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(j)

		// The first trigger starts a run that blocks.
		go wrappedJob.Run()
		<-j.started

		// Nine more triggers arrive while the first run is blocked. Each
		// merged trigger returns immediately rather than queuing.
		for i := 0; i < 9; i++ {
			merged := make(chan struct{})
			go func() {
				wrappedJob.Run()
				close(merged)
			}()
			select {
			case <-merged:
			case <-time.After(time.Second):
				t.Fatal("expected merged trigger to return immediately")
			}
		}
		if c := j.Runs(); c != 1 {
			t.Fatalf("expected only the first run started, got %d", c)
		}

		// Releasing the first run causes exactly one merged follow-up run.
		j.release <- struct{}{}
		<-j.started
		j.release <- struct{}{}

		// No further run starts: 10 triggers produced exactly 2 runs.
		select {
		case <-j.started:
			t.Fatal("expected no third run, got one")
		case <-time.After(50 * time.Millisecond):
		}
		if c := j.Runs(); c != 2 {
			t.Errorf("expected 10 triggers to produce 2 runs, got %d", c)
		}
	})

	t.Run("pending run uses most recent trigger time", func(t *testing.T) {
		j := newBlockingJob()
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(j)

		go wrappedJob.Run()
		<-j.started

		// Fire several merged triggers; record the moment just before the
		// last one so we can prove the pending run observed the most recent
		// trigger rather than an earlier one.
		var beforeLast time.Time
		for i := 0; i < 5; i++ {
			// Space the triggers apart so their timestamps are distinct even
			// on platforms with a coarse system clock.
			time.Sleep(5 * time.Millisecond)
			merged := make(chan struct{})
			if i == 4 {
				beforeLast = time.Now()
			}
			go func() {
				wrappedJob.Run()
				close(merged)
			}()
			<-merged
		}

		j.release <- struct{}{}
		<-j.started
		j.release <- struct{}{}

		if c := j.Runs(); c != 2 {
			t.Fatalf("expected 2 runs, got %d", c)
		}
		if got := j.Trigger(1); got.Before(beforeLast) {
			t.Errorf("expected pending run to use the most recent trigger time (>= %v), got %v", beforeLast, got)
		}
		if got, want := j.Trigger(1), j.Trigger(0); !got.After(want) {
			t.Errorf("expected pending trigger %v to be later than first trigger %v", got, want)
		}
	})

	t.Run("jobs without TriggeredJob still run", func(t *testing.T) {
		var j countJob
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(&j)
		go wrappedJob.Run()
		time.Sleep(2 * time.Millisecond)
		if c := j.Done(); c != 1 {
			t.Errorf("expected plain job run once, got %d", c)
		}
	})

	t.Run("panic recovered, pending run still executes", func(t *testing.T) {
		j := newBlockingJob()
		var runs int
		panicking := FuncJob(func() {
			j.m.Lock()
			runs++
			n := runs
			j.m.Unlock()
			j.started <- struct{}{}
			<-j.release
			if n == 1 {
				panic("boom")
			}
		})
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(panicking)

		// First run blocks, then panics when released.
		go wrappedJob.Run()
		<-j.started

		// A trigger arrives while the first run is blocked.
		merged := make(chan struct{})
		go func() {
			wrappedJob.Run()
			close(merged)
		}()
		<-merged

		// The panic is recovered and the merged pending run still executes.
		j.release <- struct{}{}
		select {
		case <-j.started:
		case <-time.After(time.Second):
			t.Fatal("expected pending run to execute after recovered panic")
		}
		j.release <- struct{}{}

		// The wrapper is not stuck: a later trigger starts a fresh run.
		time.Sleep(10 * time.Millisecond)
		go wrappedJob.Run()
		select {
		case <-j.started:
		case <-time.After(time.Second):
			t.Fatal("expected wrapper to accept new triggers after panic")
		}
		j.release <- struct{}{}
	})

	t.Run("triggers racing run completion never get stuck", func(t *testing.T) {
		var j countJob
		j.delay = time.Millisecond
		wrappedJob := NewChain(MergeIfStillRunning(DiscardLogger)).Then(&j)

		// Fire bursts of triggers that race against the completion of the
		// in-flight runs.
		for burst := 0; burst < 10; burst++ {
			for i := 0; i < 5; i++ {
				go wrappedJob.Run()
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // quiesce
		started, done := j.Started(), j.Done()
		if started != done {
			t.Errorf("expected all started runs to complete, got started=%d done=%d", started, done)
		}

		// The wrapper is not stuck in the running state: a fresh trigger
		// after quiescing runs to completion exactly once.
		go wrappedJob.Run()
		time.Sleep(10 * time.Millisecond)
		if got := j.Done(); got != done+1 {
			t.Errorf("expected exactly one more run after quiescing, got %d (want %d)", got, done+1)
		}
	})

}
