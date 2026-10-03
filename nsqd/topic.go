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
	messageCount uint64
	messageBytes uint64

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

	// pendingBytes counts message body bytes still held by the topic queue
	// (memory + backend). reservedBytes counts bytes that publish requests
	// have reserved against the topic budget but not yet fully delivered to
	// the channels that existed at publish time (it includes the topic
	// share of messages still in the topic queue and their fan-out shares).
	//
	// Both fields are guarded by budgetMu. They track live in-process state
	// and start at zero after a restart, so messages already on disk are
	// counted again only when they are re-published after recovery.
	pendingBytes  int64
	reservedBytes int64
	budgetMu      sync.Mutex

	ephemeral      bool
	deleteCallback func(*Topic)
	deleter        sync.Once

	paused    int32
	pauseChan chan int

	nsqd *NSQD
}

// ErrPendingBudgetExceeded is returned when a publish would cause the topic's
// pending byte budget to be exceeded
var ErrPendingBudgetExceeded = errors.New("topic pending byte budget exceeded")

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
	t.Unlock()

	// any fan-out bytes still reserved for messages queued in the topic when
	// the channel was deleted will never be delivered; reclaim them
	t.budgetMu.Lock()
	pendingReleased, reservedReleased := channel.reclaimLocked()
	t.subPendingLocked(pendingReleased)
	t.subReservedLocked(reservedReleased)
	t.budgetMu.Unlock()

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

	// reserve the topic share plus the fan-out cost of every channel that
	// currently exists; this guarantees concurrent publishers can never
	// over-subscribe the pending byte budget
	size := int64(len(m.Body))
	t.budgetMu.Lock()
	channels, err := t.reserveLocked(size)
	if err != nil {
		t.budgetMu.Unlock()
		return err
	}
	t.budgetMu.Unlock()

	m.publishTS = time.Now().UnixNano()
	dropped, err := t.put(m)
	if err != nil {
		// write failed (or the message was dropped): return the reservation
		t.budgetMu.Lock()
		t.releaseReservationLocked(size, channels)
		t.budgetMu.Unlock()
		return err
	}
	if dropped {
		// ephemeral topic with a full queue: nothing was enqueued
		t.budgetMu.Lock()
		t.releaseReservationLocked(size, channels)
		t.budgetMu.Unlock()
		atomic.AddUint64(&t.messageCount, 1)
		atomic.AddUint64(&t.messageBytes, uint64(len(m.Body)))
		return nil
	}

	// the topic share is now real bytes sitting in the topic queue
	t.budgetMu.Lock()
	t.addPendingLocked(size)
	t.subReservedLocked(size)
	t.budgetMu.Unlock()

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

	if len(msgs) == 0 {
		return nil
	}

	// admit the whole batch up-front, before any message is written, so that a
	// rejected batch never reaches any channel
	totalSize := int64(0)
	for _, m := range msgs {
		totalSize += int64(len(m.Body))
	}
	t.budgetMu.Lock()
	channels, err := t.reserveLocked(totalSize)
	if err != nil {
		t.budgetMu.Unlock()
		return err
	}
	t.budgetMu.Unlock()

	committedBytes := int64(0)
	for i, m := range msgs {
		size := int64(len(m.Body))
		m.publishTS = time.Now().UnixNano()
		dropped, pErr := t.put(m)
		if pErr != nil {
			err = pErr
		}
		if err != nil {
			// give back the reservations for every message not written
			t.budgetMu.Lock()
			t.releaseReservationLocked(totalSize-committedBytes, channels)
			t.budgetMu.Unlock()
			atomic.AddUint64(&t.messageCount, uint64(i))
			atomic.AddUint64(&t.messageBytes, uint64(committedBytes))
			return err
		}
		if dropped {
			// ephemeral topic with a full queue: release this message's
			// entire reservation, nothing was enqueued
			t.budgetMu.Lock()
			t.releaseReservationLocked(size, channels)
			t.budgetMu.Unlock()
			committedBytes += 0
		} else {
			t.budgetMu.Lock()
			t.addPendingLocked(size)
			t.subReservedLocked(size)
			t.budgetMu.Unlock()
			committedBytes += size
		}
	}

	atomic.AddUint64(&t.messageBytes, uint64(committedBytes))
	atomic.AddUint64(&t.messageCount, uint64(len(msgs)))
	return nil
}

// put writes a Message to the topic queue. It returns dropped=true when the
// message was silently discarded (ephemeral topic with a full memory queue);
// in that case nothing is enqueued and the reservation must be returned.
func (t *Topic) put(m *Message) (dropped bool, err error) {
	// If mem-queue-size == 0, avoid memory chan, for more consistent ordering,
	// but try to use memory chan for deferred messages (they lose deferred timer
	// in backend queue) or if topic is ephemeral (there is no backend queue).
	if cap(t.memoryMsgChan) > 0 || t.ephemeral || m.deferred != 0 {
		select {
		case t.memoryMsgChan <- m:
			return false, nil
		default:
			break // write to backend
		}
	}
	if t.ephemeral {
		// ephemeral topics have no backend queue: a full memory queue drops the
		// message
		return true, nil
	}
	err = writeMessageToBackend(m, t.backend)
	t.nsqd.SetHealth(err)
	if err != nil {
		t.nsqd.logf(LOG_ERROR,
			"TOPIC(%s) ERROR: failed to write message to backend - %s",
			t.name, err)
		return false, err
	}
	return false, nil
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
	for _, c := range t.channelMap {
		chans = append(chans, c)
	}
	t.RUnlock()
	if len(chans) > 0 && !t.IsPaused() {
		memoryMsgChan = t.memoryMsgChan
		backendChan = t.backend.ReadChan()
	}

	// main message loop
	for {
		select {
		case msg = <-memoryMsgChan:
			t.budgetMu.Lock()
			t.subPendingLocked(int64(len(msg.Body)))
			t.budgetMu.Unlock()
		case buf = <-backendChan:
			msg, err = decodeMessage(buf)
			if err != nil {
				t.nsqd.logf(LOG_ERROR, "failed to decode message - %s", err)
				continue
			}
			// backend messages don't carry the in-memory-only publishTS; the
			// message timestamp is the original publish time and is enough to
			// decide channel membership
			msg.publishTS = msg.Timestamp
			t.budgetMu.Lock()
			t.subPendingLocked(int64(len(msg.Body)))
			t.budgetMu.Unlock()
		case <-t.channelUpdateChan:
			chans = chans[:0]
			t.RLock()
			for _, c := range t.channelMap {
				chans = append(chans, c)
			}
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
				chanMsg.publishTS = msg.publishTS
			}
			if err := channel.deliverMessage(chanMsg); err != nil {
				t.nsqd.logf(LOG_ERROR,
					"TOPIC(%s) ERROR: failed to put msg(%s) to channel(%s) - %s",
					t.name, msg.ID, channel.name, err)
			}
		}
	}

exit:
	t.nsqd.logf(LOG_INFO, "TOPIC(%s): closing ... messagePump", t.name)
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
	t.RLock()
	channels := make([]*Channel, 0, len(t.channelMap))
	for _, c := range t.channelMap {
		channels = append(channels, c)
	}
	t.RUnlock()

	t.budgetMu.Lock()
	for {
		select {
		case msg := <-t.memoryMsgChan:
			// the topic queue is drained; fan-out shares for these messages
			// will never be delivered, reclaim them
			t.reservedBytes -= int64(len(msg.Body))
		default:
			goto finish
		}
	}

finish:
	t.reservedBytes -= t.pendingBytes
	t.pendingBytes = 0
	if t.reservedBytes < 0 {
		t.reservedBytes = 0
	}
	// fan-out bytes still reserved for queued-but-now-deleted messages in
	// existing channels must also be released
	for _, c := range channels {
		if c.reservedBytes > 0 {
			t.reservedBytes -= c.reservedBytes
			c.reservedBytes = 0
		}
	}
	if t.reservedBytes < 0 {
		t.reservedBytes = 0
	}
	t.budgetMu.Unlock()

	return t.backend.Empty()
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

// addPendingLocked increases the topic's pending byte counter; the caller
// must hold t.budgetMu
func (t *Topic) addPendingLocked(n int64) {
	t.pendingBytes += n
}

// subPendingLocked decreases the topic's pending byte counter, clamping at
// zero; the caller must hold t.budgetMu
func (t *Topic) subPendingLocked(n int64) {
	t.pendingBytes -= n
	if t.pendingBytes < 0 {
		t.pendingBytes = 0
	}
}

// subReservedLocked decreases the topic's reserved byte counter, clamping at
// zero; the caller must hold t.budgetMu
func (t *Topic) subReservedLocked(n int64) {
	t.reservedBytes -= n
	if t.reservedBytes < 0 {
		t.reservedBytes = 0
	}
}

// channelPendingLocked returns the sum of pending bytes currently owned by
// the topic's channels; the caller must hold t.budgetMu
func (t *Topic) channelPendingLocked() int64 {
	var total int64
	for _, c := range t.channelMap {
		total += c.pendingBytes
	}
	return total
}

// totalPendingLocked returns the topic's total liability: bytes held in the
// topic queue (memory + backend), bytes held by every channel and bytes
// reserved but not yet committed; the caller must hold t.budgetMu
func (t *Topic) totalPendingLocked() int64 {
	return t.pendingBytes + t.channelPendingLocked() + t.reservedBytes
}

// reserveLocked atomically reserves size bytes for the topic share plus the
// fan-out cost across all channels that currently exist. It returns the
// channels that received a share. The caller must hold t.budgetMu.
func (t *Topic) reserveLocked(size int64) ([]*Channel, error) {
	limit := t.nsqd.getOpts().MaxTopicPendingBytes
	if limit > 0 {
		fanOut := int64(0)
		channels := make([]*Channel, 0, len(t.channelMap))
		for _, c := range t.channelMap {
			fanOut += size
			channels = append(channels, c)
		}
		ask := size + fanOut
		if t.totalPendingLocked()+ask > limit {
			return nil, ErrPendingBudgetExceeded
		}
		t.reservedBytes += ask
		for _, c := range channels {
			c.reservedBytes += size
		}
		return channels, nil
	}

	// no limit configured: still track reservations so fan-out accounting
	// and stats remain accurate
	channels := make([]*Channel, 0, len(t.channelMap))
	for _, c := range t.channelMap {
		channels = append(channels, c)
	}
	fanOut := int64(len(channels)) * size
	t.reservedBytes += size + fanOut
	for _, c := range channels {
		c.reservedBytes += size
	}
	return channels, nil
}

// releaseReservationLocked returns topicShare bytes of topic reservation
// plus the corresponding fan-out shares of the listed channels; the caller
// must hold t.budgetMu
func (t *Topic) releaseReservationLocked(topicShare int64, channels []*Channel) {
	t.subReservedLocked(topicShare)
	for _, c := range channels {
		if c.reservedBytes >= topicShare {
			c.reservedBytes -= topicShare
			t.subReservedLocked(topicShare)
		}
	}
}

// releaseChannelReservationLocked gives back a single fan-out share that
// could not be delivered (e.g. an ephemeral channel dropped the message);
// the caller must hold t.budgetMu
func (t *Topic) releaseChannelReservationLocked(c *Channel, size int64) {
	if c.reservedBytes >= size {
		c.reservedBytes -= size
		t.subReservedLocked(size)
	}
}

// PendingUsage reports the topic's used bytes (topic queue plus all channel
// fan-out costs), the bytes still held by the topic queue alone and the
// bytes reserved by in-flight publish requests.
func (t *Topic) PendingUsage() (used int64, topicQueued int64, reserved int64) {
	t.budgetMu.Lock()
	used = t.pendingBytes + t.channelPendingLocked()
	topicQueued = t.pendingBytes
	reserved = t.reservedBytes
	t.budgetMu.Unlock()
	return used, topicQueued, reserved
}

// ReservedBytesForChannel returns the fan-out bytes currently reserved for
// the given channel
func (t *Topic) ReservedBytesForChannel(c *Channel) int64 {
	t.budgetMu.Lock()
	v := c.reservedBytes
	t.budgetMu.Unlock()
	return v
}
