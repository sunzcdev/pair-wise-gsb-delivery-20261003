package nsqd

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsqio/go-diskqueue"
	"github.com/nsqio/nsq/internal/lg"
	"github.com/nsqio/nsq/internal/quantile"
	"github.com/nsqio/nsq/internal/util"
)

type Topic struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	messageCount    uint64
	messageBytes    uint64
	topicPending    int64
	fanoutReserved  int64
	channelPending  int64
	pendingReserved int64
	maxPendingBytes int64

	sync.RWMutex

	name              string
	channelMap        map[string]*Channel
	backend           BackendQueue
	memoryMsgChan     chan *Message
	startChan         chan int
	exitChan          chan int
	channelUpdateChan chan int
	waitGroup         util.WaitGroupWrapper
	exitFlag          int32
	idFactory         *guidFactory

	// pending byte budget accounting (in message body bytes), maintained
	// with 64bit atomics:
	//   topicPending    - bytes held in the topic's own (memory/backend) queue
	//   fanoutReserved  - bytes promised as per-channel copies that have not
	//                     been handed to a channel yet; includes the copies
	//                     of topic-held backlog and, for the message the pump
	//                     is currently fanning out, the copies not yet delivered
	//   channelPending  - bytes actually held by channels (queued, in-flight,
	//                     deferred), equal to the sum of channel.pendingBytes
	//   pendingReserved - bytes reserved by publish requests still in flight
	//                     (accepted by the budget check but not committed)

	ephemeral      bool
	deleteCallback func(*Topic)
	deleter        sync.Once

	paused    int32
	pauseChan chan int

	nsqd *NSQD
}

// Topic constructor
func NewTopic(topicName string, nsqd *NSQD, deleteCallback func(*Topic)) *Topic {
	t := &Topic{
		name:              topicName,
		channelMap:        make(map[string]*Channel),
		memoryMsgChan:     make(chan *Message, nsqd.getOpts().MemQueueSize),
		startChan:         make(chan int, 1),
		exitChan:          make(chan int),
		channelUpdateChan: make(chan int),
		nsqd:              nsqd,
		paused:            0,
		pauseChan:         make(chan int),
		deleteCallback:    deleteCallback,
		idFactory:         NewGUIDFactory(nsqd.getOpts().ID),
	}
	atomic.StoreInt64(&t.maxPendingBytes, nsqd.getOpts().MaxTopicPendingBytes)
	if strings.HasSuffix(topicName, "#ephemeral") {
		t.ephemeral = true
		t.backend = newDummyBackendQueue()
	} else {
		dqLogf := func(level diskqueue.LogLevel, f string, args ...interface{}) {
			opts := nsqd.getOpts()
			lg.Logf(opts.Logger, opts.LogLevel, lg.LogLevel(level), f, args...)
		}
		t.backend = diskqueue.New(
			topicName,
			nsqd.getOpts().DataPath,
			nsqd.getOpts().MaxBytesPerFile,
			int32(minValidMsgLength),
			int32(nsqd.getOpts().MaxMsgSize)+minValidMsgLength,
			nsqd.getOpts().SyncEvery,
			nsqd.getOpts().SyncTimeout,
			dqLogf,
		)
	}

	t.waitGroup.Wrap(t.messagePump)

	t.nsqd.Notify(t, !t.ephemeral)

	return t
}

func (t *Topic) Start() {
	select {
	case t.startChan <- 1:
	default:
	}
}

// Exiting returns a boolean indicating if this topic is closed/exiting
func (t *Topic) Exiting() bool {
	return atomic.LoadInt32(&t.exitFlag) == 1
}

// GetChannel performs a thread safe operation
// to return a pointer to a Channel object (potentially new)
// for the given Topic
func (t *Topic) GetChannel(channelName string) *Channel {
	t.Lock()
	channel, isNew := t.getOrCreateChannel(channelName)
	t.Unlock()

	if isNew {
		// update messagePump state
		select {
		case t.channelUpdateChan <- 1:
		case <-t.exitChan:
		}
	}

	return channel
}

// this expects the caller to handle locking
func (t *Topic) getOrCreateChannel(channelName string) (*Channel, bool) {
	channel, ok := t.channelMap[channelName]
	if !ok {
		deleteCallback := func(c *Channel) {
			_ = t.DeleteExistingChannel(c.name)
		}
		channel = NewChannel(t, channelName, deleteCallback)
		t.channelMap[channelName] = channel
		// one new copy of every topic-held message is now promised to
		// this channel (NSQ backlog semantics; not admission-controlled)
		t.onChannelAdded()
		t.nsqd.logf(LOG_INFO, "TOPIC(%s): new channel(%s)", t.name, channel.name)
		return channel, true
	}
	return channel, false
}

func (t *Topic) GetExistingChannel(channelName string) (*Channel, error) {
	t.RLock()
	defer t.RUnlock()
	channel, ok := t.channelMap[channelName]
	if !ok {
		return nil, errors.New("channel does not exist")
	}
	return channel, nil
}

// DeleteExistingChannel removes a channel from the topic only if it exists
func (t *Topic) DeleteExistingChannel(channelName string) error {
	t.RLock()
	channel, ok := t.channelMap[channelName]
	t.RUnlock()
	if !ok {
		return errors.New("channel does not exist")
	}

	t.nsqd.logf(LOG_INFO, "TOPIC(%s): deleting channel %s", t.name, channel.name)

	// delete empties the channel before closing
	// (so that we dont leave any messages around)
	//
	// we do this before removing the channel from map below (with no lock)
	// so that any incoming subs will error and not create a new channel
	// to enforce ordering
	err := channel.Delete()
	if err != nil {
		return err
	}

	t.Lock()
	delete(t.channelMap, channelName)
	numChannels := len(t.channelMap)
	t.onChannelRemoved(channel)
	t.Unlock()

	// update messagePump state
	select {
	case t.channelUpdateChan <- 1:
	case <-t.exitChan:
	}

	if numChannels == 0 && t.ephemeral {
		go t.deleter.Do(func() { t.deleteCallback(t) })
	}

	return nil
}

// PutMessage writes a Message to the queue
func (t *Topic) PutMessage(m *Message) error {
	t.RLock()
	defer t.RUnlock()
	if atomic.LoadInt32(&t.exitFlag) == 1 {
		return errors.New("exiting")
	}
	numChannels := len(t.channelMap)
	cost := int64(len(m.Body)) * int64(numChannels+1)
	if !t.reservePending(cost) {
		return ErrPendingByteBudgetExceeded
	}
	err := t.put(m)
	if err != nil {
		t.releasePending(cost)
		return err
	}
	t.commitPending(int64(len(m.Body)), int64(len(m.Body))*int64(numChannels))
	atomic.AddUint64(&t.messageCount, 1)
	atomic.AddUint64(&t.messageBytes, uint64(len(m.Body)))
	return nil
}

// PutMessages writes multiple Messages to the queue
func (t *Topic) PutMessages(msgs []*Message) error {
	t.RLock()
	defer t.RUnlock()
	if atomic.LoadInt32(&t.exitFlag) == 1 {
		return errors.New("exiting")
	}

	messageTotalBytes := 0
	numChannels := len(t.channelMap)

	var cost int64
	for _, m := range msgs {
		cost += int64(len(m.Body)) * int64(numChannels+1)
	}
	if !t.reservePending(cost) {
		return ErrPendingByteBudgetExceeded
	}

	var committed int64
	for i, m := range msgs {
		err := t.put(m)
		if err != nil {
			t.releasePending(cost - committed)
			atomic.AddUint64(&t.messageCount, uint64(i))
			atomic.AddUint64(&t.messageBytes, uint64(messageTotalBytes))
			return err
		}
		bodyBytes := int64(len(m.Body))
		t.commitPending(bodyBytes, bodyBytes*int64(numChannels))
		committed += bodyBytes * int64(numChannels+1)
		messageTotalBytes += len(m.Body)
	}

	atomic.AddUint64(&t.messageBytes, uint64(messageTotalBytes))
	atomic.AddUint64(&t.messageCount, uint64(len(msgs)))
	return nil
}

// ErrPendingByteBudgetExceeded is returned by a publish when accepting it
// would exceed the topic's configured pending byte budget.
var ErrPendingByteBudgetExceeded = errors.New("pending byte budget exceeded")

// NumChannels returns the current number of channels on the topic.
func (t *Topic) NumChannels() int {
	t.RLock()
	n := len(t.channelMap)
	t.RUnlock()
	return n
}

// reservePending atomically reserves cost bytes against the topic's
// pending byte budget. It returns false (and reserves nothing) when the
// budget is configured and would be exceeded.
func (t *Topic) reservePending(cost int64) bool {
	if atomic.LoadInt64(&t.maxPendingBytes) > 0 {
		for {
			reserved := atomic.LoadInt64(&t.pendingReserved)
			used := atomic.LoadInt64(&t.topicPending) +
				atomic.LoadInt64(&t.fanoutReserved) +
				atomic.LoadInt64(&t.channelPending)
			if used+reserved+cost > atomic.LoadInt64(&t.maxPendingBytes) {
				return false
			}
			if atomic.CompareAndSwapInt64(&t.pendingReserved, reserved, reserved+cost) {
				break
			}
		}
	} else {
		atomic.AddInt64(&t.pendingReserved, cost)
	}
	return true
}

// commitPending moves cost bytes from the in-flight reservation into the
// pending accounting after a message was accepted by the topic queue.
// topicBytes is the topic's own copy and fanoutBytes is the sum of the
// per-channel copies.
func (t *Topic) commitPending(topicBytes, fanoutBytes int64) {
	atomic.AddInt64(&t.pendingReserved, -(topicBytes + fanoutBytes))
	atomic.AddInt64(&t.topicPending, topicBytes)
	atomic.AddInt64(&t.fanoutReserved, fanoutBytes)
}

// releasePending gives back a reservation that was not (or will not be)
// committed, e.g. on validation, backend write or fan-out failure.
func (t *Topic) releasePending(cost int64) {
	atomic.AddInt64(&t.pendingReserved, -cost)
}

// dequeuePending moves a dequeued message's bytes out of the topic queue.
// Its fan-out reservation stays booked while the pump fans the message
// out: each successful delivery releases one copy, and undelivered
// copies (fan-out failures) are released afterwards by the pump.
func (t *Topic) dequeuePending(bodyBytes int64) {
	atomicSubFloor(&t.topicPending, bodyBytes)
}

// onChannelAdded reserves one additional copy of every message still
// held by the topic for a newly created channel. Per NSQ semantics, this
// backlog delivery is not admission-controlled, so it is charged
// directly rather than rejected.
func (t *Topic) onChannelAdded() {
	atomic.AddInt64(&t.fanoutReserved, atomic.LoadInt64(&t.topicPending))
}

// onChannelRemoved releases every outstanding reservation attributable
// to the removed channel: one copy of each topic-held message plus
// everything the channel itself held.
func (t *Topic) onChannelRemoved(c *Channel) {
	atomicSubFloor(&t.fanoutReserved, atomic.LoadInt64(&t.topicPending))
	atomicSubFloor(&t.channelPending, c.PendingBytes())
}

// releaseChannelPending releases bytes no longer held by a channel
// (consumer finish, channel empty/delete).
func (t *Topic) releaseChannelPending(n int64) {
	atomicSubFloor(&t.channelPending, n)
}

// releaseFanoutReservation returns fan-out bytes that never reached a
// channel (fan-out failures).
func (t *Topic) releaseFanoutReservation(n int64) {
	atomicSubFloor(&t.fanoutReserved, n)
}

// atomicSubFloor subtracts n from *addr, never allowing it to become
// negative (guards admission against any transient accounting drift).
func atomicSubFloor(addr *int64, n int64) {
	for {
		v := atomic.LoadInt64(addr)
		nv := v - n
		if nv < 0 {
			nv = 0
		}
		if atomic.CompareAndSwapInt64(addr, v, nv) {
			return
		}
	}
}

// addChannelPending accounts for bytes newly held by a channel through a
// path that did not have a fan-out reservation (requeue/deferral of
// in-flight messages, channel-side timeouts).
func (t *Topic) addChannelPending(n int64) {
	atomic.AddInt64(&t.channelPending, n)
}

// PendingByteStats returns the bytes currently held (including fan-out),
// the bytes reserved by in-flight publish requests, and the configured
// budget (0 = unlimited).
func (t *Topic) PendingByteStats() (used, reserved, limit int64) {
	used = atomic.LoadInt64(&t.topicPending) +
		atomic.LoadInt64(&t.fanoutReserved) +
		atomic.LoadInt64(&t.channelPending)
	reserved = atomic.LoadInt64(&t.pendingReserved)
	limit = atomic.LoadInt64(&t.maxPendingBytes)
	return
}

func (t *Topic) put(m *Message) error {
	// If mem-queue-size == 0, avoid memory chan, for more consistent ordering,
	// but try to use memory chan for deferred messages (they lose deferred timer
	// in backend queue) or if topic is ephemeral (there is no backend queue).
	if cap(t.memoryMsgChan) > 0 || t.ephemeral || m.deferred != 0 {
		select {
		case t.memoryMsgChan <- m:
			return nil
		default:
			break // write to backend
		}
	}
	err := writeMessageToBackend(m, t.backend)
	t.nsqd.SetHealth(err)
	if err != nil {
		t.nsqd.logf(LOG_ERROR,
			"TOPIC(%s) ERROR: failed to write message to backend - %s",
			t.name, err)
		return err
	}
	return nil
}

func (t *Topic) Depth() int64 {
	return int64(len(t.memoryMsgChan)) + t.backend.Depth()
}

// messagePump selects over the in-memory and backend queue and
// writes messages to every channel for this topic
func (t *Topic) messagePump() {
	var msg *Message
	var buf []byte
	var err error
	var chans []*Channel
	var memoryMsgChan chan *Message
	var backendChan <-chan []byte

	// do not pass messages before Start(), but avoid blocking Pause() or GetChannel()
	for {
		select {
		case <-t.channelUpdateChan:
			continue
		case <-t.pauseChan:
			continue
		case <-t.exitChan:
			goto exit
		case <-t.startChan:
		}
		break
	}
	t.RLock()
	chans = t.snapshotChannelsLocked(chans)
	t.RUnlock()
	if len(chans) > 0 && !t.IsPaused() {
		memoryMsgChan = t.memoryMsgChan
		backendChan = t.backend.ReadChan()
	}

	// main message loop
	for {
		select {
		case msg = <-memoryMsgChan:
		case buf = <-backendChan:
			msg, err = decodeMessage(buf)
			if err != nil {
				t.nsqd.logf(LOG_ERROR, "failed to decode message - %s", err)
				continue
			}
		case <-t.channelUpdateChan:
			chans = chans[:0]
			t.RLock()
			chans = t.snapshotChannelsLocked(chans)
			t.RUnlock()
			if len(chans) == 0 || t.IsPaused() {
				memoryMsgChan = nil
				backendChan = nil
			} else {
				memoryMsgChan = t.memoryMsgChan
				backendChan = t.backend.ReadChan()
			}
			continue
		case <-t.pauseChan:
			if len(chans) == 0 || t.IsPaused() {
				memoryMsgChan = nil
				backendChan = nil
			} else {
				memoryMsgChan = t.memoryMsgChan
				backendChan = t.backend.ReadChan()
			}
			continue
		case <-t.exitChan:
			goto exit
		}

		numChannels := len(chans)
		t.dequeuePending(int64(len(msg.Body)))

		bodyLen := int64(len(msg.Body))
		for i, channel := range chans {
			chanMsg := msg
			// copy the message because each channel
			// needs a unique instance but...
			// fastpath to avoid copy if its the first channel
			// (the topic already created the first copy)
			if i > 0 {
				chanMsg = NewMessage(msg.ID, msg.Body)
				chanMsg.Timestamp = msg.Timestamp
				chanMsg.deferred = msg.deferred
			}
			if chanMsg.deferred != 0 {
				if err := channel.PutMessageDeferred(chanMsg, chanMsg.deferred); err != nil {
					t.nsqd.logf(LOG_ERROR,
						"TOPIC(%s) ERROR: failed to defer msg(%s) to channel(%s) - %s",
						t.name, msg.ID, channel.name, err)
					continue
				}
				continue
			}
			err := channel.PutMessage(chanMsg)
			if err != nil {
				t.nsqd.logf(LOG_ERROR,
					"TOPIC(%s) ERROR: failed to put msg(%s) to channel(%s) - %s",
					t.name, msg.ID, channel.name, err)
			}
		}
		// release the whole fan-out reservation for this message; any copy
		// that failed to reach a channel was never charged to channelPending
		t.releaseFanoutReservation(bodyLen * int64(numChannels))
	}

exit:
	t.nsqd.logf(LOG_INFO, "TOPIC(%s): closing ... messagePump", t.name)
}

// snapshotChannelsLocked appends the topic's current channels to chans
// (which is truncated by the caller beforehand); the caller must hold t.
func (t *Topic) snapshotChannelsLocked(chans []*Channel) []*Channel {
	for _, c := range t.channelMap {
		chans = append(chans, c)
	}
	return chans
}

// Delete empties the topic and all its channels and closes
func (t *Topic) Delete() error {
	return t.exit(true)
}

// Close persists all outstanding topic data and closes all its channels
func (t *Topic) Close() error {
	return t.exit(false)
}

func (t *Topic) exit(deleted bool) error {
	if !atomic.CompareAndSwapInt32(&t.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		t.nsqd.logf(LOG_INFO, "TOPIC(%s): deleting", t.name)

		// since we are explicitly deleting a topic (not just at system exit time)
		// de-register this from the lookupd
		t.nsqd.Notify(t, !t.ephemeral)
	} else {
		t.nsqd.logf(LOG_INFO, "TOPIC(%s): closing", t.name)
	}

	close(t.exitChan)

	// synchronize the close of messagePump()
	t.waitGroup.Wait()

	if deleted {
		t.Lock()
		for _, channel := range t.channelMap {
			delete(t.channelMap, channel.name)
			_ = channel.Delete()
		}
		t.Unlock()

		// empty the queue (deletes the backend files, too)
		err := t.Empty()
		if err != nil {
			return err
		}
		return t.backend.Delete()
	}

	// close all the channels
	t.RLock()
	for _, channel := range t.channelMap {
		err := channel.Close()
		if err != nil {
			// we need to continue regardless of error to close all the channels
			t.nsqd.logf(LOG_ERROR, "channel(%s) close - %s", channel.name, err)
		}
	}
	t.RUnlock()

	// write anything leftover to disk
	err := t.flush()
	if err != nil {
		return err
	}
	return t.backend.Close()
}

func (t *Topic) Empty() error {
	for {
		select {
		case <-t.memoryMsgChan:
		default:
			goto finish
		}
	}

finish:
	// recompute the pending budget from the messages still held by
	// channels; topic backlog was drained above (best-effort under
	// concurrent publishes)
	t.resyncPending()
	return t.backend.Empty()
}

// resyncPending recomputes the pending byte counters from the pending
// bytes reported by each channel and the topic's own queue.
func (t *Topic) resyncPending() {
	var channelHeld int64
	t.RLock()
	n := len(t.channelMap)
	for _, c := range t.channelMap {
		channelHeld += c.PendingBytes()
	}
	topicHeld := int64(len(t.memoryMsgChan))
	t.RUnlock()
	atomic.StoreInt64(&t.fanoutReserved, topicHeld*int64(n))
	atomic.StoreInt64(&t.channelPending, channelHeld)
	atomic.StoreInt64(&t.topicPending, topicHeld)
}

func (t *Topic) flush() error {
	if len(t.memoryMsgChan) > 0 {
		t.nsqd.logf(LOG_INFO,
			"TOPIC(%s): flushing %d memory messages to backend",
			t.name, len(t.memoryMsgChan))
	}

	for {
		select {
		case msg := <-t.memoryMsgChan:
			err := writeMessageToBackend(msg, t.backend)
			if err != nil {
				t.nsqd.logf(LOG_ERROR,
					"ERROR: failed to write message to backend - %s", err)
			}
		default:
			goto finish
		}
	}

finish:
	return nil
}

func (t *Topic) AggregateChannelE2eProcessingLatency() *quantile.Quantile {
	var latencyStream *quantile.Quantile
	t.RLock()
	realChannels := make([]*Channel, 0, len(t.channelMap))
	for _, c := range t.channelMap {
		realChannels = append(realChannels, c)
	}
	t.RUnlock()
	for _, c := range realChannels {
		if c.e2eProcessingLatencyStream == nil {
			continue
		}
		if latencyStream == nil {
			latencyStream = quantile.New(
				t.nsqd.getOpts().E2EProcessingLatencyWindowTime,
				t.nsqd.getOpts().E2EProcessingLatencyPercentiles)
		}
		latencyStream.Merge(c.e2eProcessingLatencyStream)
	}
	return latencyStream
}

func (t *Topic) Pause() error {
	return t.doPause(true)
}

func (t *Topic) UnPause() error {
	return t.doPause(false)
}

func (t *Topic) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&t.paused, 1)
	} else {
		atomic.StoreInt32(&t.paused, 0)
	}

	select {
	case t.pauseChan <- 1:
	case <-t.exitChan:
	}

	return nil
}

func (t *Topic) IsPaused() bool {
	return atomic.LoadInt32(&t.paused) == 1
}

func (t *Topic) GenerateID() MessageID {
	var i int64 = 0
	for {
		id, err := t.idFactory.NewGUID()
		if err == nil {
			return id.Hex()
		}
		if i%10000 == 0 {
			t.nsqd.logf(LOG_ERROR, "TOPIC(%s): failed to create guid - %s", t.name, err)
		}
		time.Sleep(time.Millisecond)
		i++
	}
}
