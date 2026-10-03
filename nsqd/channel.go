package nsqd

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsqio/go-diskqueue"

	"github.com/nsqio/nsq/internal/lg"
	"github.com/nsqio/nsq/internal/pqueue"
	"github.com/nsqio/nsq/internal/quantile"
)

type Consumer interface {
	UnPause()
	Pause()
	Close() error
	TimedOutMessage()
	Stats(string) ClientStats
	Empty()
}

// Channel represents the concrete type for a NSQ channel (and also
// implements the Queue interface)
//
// There can be multiple channels per topic, each with there own unique set
// of subscribers (clients).
//
// Channels maintain all client and message metadata, orchestrating in-flight
// messages, timeouts, requeuing, etc.
type Channel struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	requeueCount        uint64
	messageCount        uint64
	zoneLocalMsgCount   uint64
	regionLocalMsgCount uint64
	globalMsgCount      uint64
	timeoutCount        uint64

	sync.RWMutex

	topicName string
	name      string
	topic     *Topic
	nsqd      *NSQD

	backend BackendQueue

	topologyAwareConsumption bool
	zoneLocalMsgChan         chan *Message
	regionLocalMsgChan       chan *Message
	memoryMsgChan            chan *Message
	exitFlag                 int32
	exitMutex                sync.RWMutex

	// pendingBytes is the number of message body bytes currently owned by
	// this channel (memory queue, backend queue, in-flight and deferred).
	// reservedBytes is the fan-out cost of messages still waiting in the
	// topic's queue that was reserved against this channel.
	//
	// Both fields are guarded by the owning topic's budget mutex.
	pendingBytes  int64
	reservedBytes int64
	// createdAt marks the channel creation time; a message published after
	// this instant had its fan-out cost reserved for this channel
	createdAt int64

	// state tracking
	clients        map[int64]Consumer
	paused         int32
	ephemeral      bool
	deleteCallback func(*Channel)
	deleter        sync.Once

	// Stats tracking
	e2eProcessingLatencyStream *quantile.Quantile

	// TODO: these can be DRYd up
	deferredMessages map[MessageID]*pqueue.Item
	deferredPQ       pqueue.PriorityQueue
	deferredMutex    sync.Mutex
	inFlightMessages map[MessageID]*Message
	inFlightPQ       inFlightPqueue
	inFlightMutex    sync.Mutex
}

// NewChannel creates a new instance of the Channel type and returns a pointer
func NewChannel(t *Topic, channelName string, deleteCallback func(*Channel)) *Channel {

	c := &Channel{
		topicName:                t.name,
		name:                     channelName,
		topic:                    t,
		memoryMsgChan:            nil,
		clients:                  make(map[int64]Consumer),
		deleteCallback:           deleteCallback,
		nsqd:                     t.nsqd,
		ephemeral:                strings.HasSuffix(channelName, "#ephemeral"),
		topologyAwareConsumption: t.nsqd.getOpts().HasExperiment(TopologyAwareConsumption),
		createdAt:                time.Now().UnixNano(),
	}

	if t.nsqd.getOpts().TopologyRegion != "" {
		c.regionLocalMsgChan = make(chan *Message)
	}
	if t.nsqd.getOpts().TopologyZone != "" {
		c.zoneLocalMsgChan = make(chan *Message)
	}

	// avoid mem-queue if size == 0 for more consistent ordering
	if t.nsqd.getOpts().MemQueueSize > 0 || c.ephemeral {
		c.memoryMsgChan = make(chan *Message, t.nsqd.getOpts().MemQueueSize)
	}
	if len(t.nsqd.getOpts().E2EProcessingLatencyPercentiles) > 0 {
		c.e2eProcessingLatencyStream = quantile.New(
			t.nsqd.getOpts().E2EProcessingLatencyWindowTime,
			t.nsqd.getOpts().E2EProcessingLatencyPercentiles,
		)
	}

	c.initPQ()

	if c.ephemeral {
		c.backend = newDummyBackendQueue()
	} else {
		dqLogf := func(level diskqueue.LogLevel, f string, args ...interface{}) {
			opts := t.nsqd.getOpts()
			lg.Logf(opts.Logger, opts.LogLevel, lg.LogLevel(level), f, args...)
		}
		// backend names, for uniqueness, automatically include the topic...
		backendName := getBackendName(t.name, channelName)
		c.backend = diskqueue.New(
			backendName,
			t.nsqd.getOpts().DataPath,
			t.nsqd.getOpts().MaxBytesPerFile,
			int32(minValidMsgLength),
			int32(t.nsqd.getOpts().MaxMsgSize)+minValidMsgLength,
			t.nsqd.getOpts().SyncEvery,
			t.nsqd.getOpts().SyncTimeout,
			dqLogf,
		)
	}

	t.nsqd.Notify(c, !c.ephemeral)

	return c
}

func (c *Channel) initPQ() {
	pqSize := int(math.Max(1, float64(c.nsqd.getOpts().MemQueueSize)/10))

	c.inFlightMutex.Lock()
	c.inFlightMessages = make(map[MessageID]*Message)
	c.inFlightPQ = newInFlightPqueue(pqSize)
	c.inFlightMutex.Unlock()

	c.deferredMutex.Lock()
	c.deferredMessages = make(map[MessageID]*pqueue.Item)
	c.deferredPQ = pqueue.New(pqSize)
	c.deferredMutex.Unlock()
}

// Exiting returns a boolean indicating if this channel is closed/exiting
func (c *Channel) Exiting() bool {
	return atomic.LoadInt32(&c.exitFlag) == 1
}

// Delete empties the channel and closes
func (c *Channel) Delete() error {
	return c.exit(true)
}

// Close cleanly closes the Channel
func (c *Channel) Close() error {
	return c.exit(false)
}

func (c *Channel) exit(deleted bool) error {
	c.exitMutex.Lock()
	defer c.exitMutex.Unlock()

	if !atomic.CompareAndSwapInt32(&c.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): deleting", c.name)

		// since we are explicitly deleting a channel (not just at system exit time)
		// de-register this from the lookupd
		c.nsqd.Notify(c, !c.ephemeral)
	} else {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): closing", c.name)
	}

	// this forceably closes client connections
	c.RLock()
	for _, client := range c.clients {
		if err := client.Close(); err != nil {
			c.nsqd.logf(LOG_ERROR, "CHANNEL(%s): failed to close client - %s", c.name, err)
		}
	}
	c.RUnlock()

	if deleted {
		// empty the queue (deletes the backend files, too)
		err := c.Empty()
		if err != nil {
			return err
		}
		return c.backend.Delete()
	}

	// write anything leftover to disk
	err := c.flush()
	if err != nil {
		return err
	}
	return c.backend.Close()
}

func (c *Channel) Empty() error {
	c.Lock()
	defer c.Unlock()

	// sum the bytes still buffered in memory or tracked in the in-flight /
	// deferred heaps; disk-backed bytes are accounted for via pendingBytes
	// when the backend is emptied below
	c.topic.budgetMu.Lock()
	drained := int64(0)
	for {
		select {
		case msg := <-c.zoneLocalMsgChan:
			drained += int64(len(msg.Body))
			continue
		case msg := <-c.regionLocalMsgChan:
			drained += int64(len(msg.Body))
			continue
		case msg := <-c.memoryMsgChan:
			drained += int64(len(msg.Body))
			continue
		default:
		}
		break
	}
	c.inFlightMutex.Lock()
	for _, msg := range c.inFlightMessages {
		drained += int64(len(msg.Body))
	}
	c.inFlightMutex.Unlock()
	c.deferredMutex.Lock()
	for _, item := range c.deferredMessages {
		msg := item.Value.(*Message)
		drained += int64(len(msg.Body))
	}
	c.deferredMutex.Unlock()

	c.initPQ()
	for _, client := range c.clients {
		client.Empty()
	}

	// the backend Empty() drops everything still queued on disk; reset all
	// accounting and return that capacity to the topic's pending budget
	err := c.backend.Empty()
	if err != nil {
		c.topic.budgetMu.Unlock()
		return err
	}
	c.subPendingLocked(drained + c.pendingBytes)
	c.topic.budgetMu.Unlock()

	return nil
}

// flush persists all the messages in internal memory buffers to the backend
// it does not drain inflight/deferred because it is only called in Close()
func (c *Channel) flush() error {
	if len(c.zoneLocalMsgChan) > 0 || len(c.regionLocalMsgChan) > 0 || len(c.memoryMsgChan) > 0 || len(c.inFlightMessages) > 0 || len(c.deferredMessages) > 0 {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): flushing %d memory %d in-flight %d deferred messages to backend",
			c.name, len(c.memoryMsgChan)+len(c.zoneLocalMsgChan)+len(c.regionLocalMsgChan), len(c.inFlightMessages), len(c.deferredMessages))
	}

	for {
		select {
		case msg := <-c.zoneLocalMsgChan:
			err := writeMessageToBackend(msg, c.backend)
			if err != nil {
				c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
			}
		case msg := <-c.regionLocalMsgChan:
			err := writeMessageToBackend(msg, c.backend)
			if err != nil {
				c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
			}
		case msg := <-c.memoryMsgChan:
			err := writeMessageToBackend(msg, c.backend)
			if err != nil {
				c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
			}
		default:
			goto finish
		}
	}

finish:
	c.inFlightMutex.Lock()
	for _, msg := range c.inFlightMessages {
		err := writeMessageToBackend(msg, c.backend)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
		}
	}
	c.inFlightMutex.Unlock()

	c.deferredMutex.Lock()
	for _, item := range c.deferredMessages {
		msg := item.Value.(*Message)
		err := writeMessageToBackend(msg, c.backend)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
		}
	}
	c.deferredMutex.Unlock()

	return nil
}

func (c *Channel) Depth() int64 {
	return int64(len(c.memoryMsgChan)) + int64(len(c.zoneLocalMsgChan)) + int64(len(c.regionLocalMsgChan)) + c.backend.Depth()
}

func (c *Channel) Pause() error {
	return c.doPause(true)
}

func (c *Channel) UnPause() error {
	return c.doPause(false)
}

func (c *Channel) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&c.paused, 1)
	} else {
		atomic.StoreInt32(&c.paused, 0)
	}

	c.RLock()
	for _, client := range c.clients {
		if pause {
			client.Pause()
		} else {
			client.UnPause()
		}
	}
	c.RUnlock()
	return nil
}

func (c *Channel) IsPaused() bool {
	return atomic.LoadInt32(&c.paused) == 1
}

// PutMessage writes a Message to the queue
func (c *Channel) PutMessage(m *Message) error {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()
	if c.Exiting() {
		return errors.New("exiting")
	}
	_, err := c.put(m)
	if err != nil {
		return err
	}
	atomic.AddUint64(&c.messageCount, 1)
	return nil
}

// deliverMessage is called by the topic messagePump for every fan-out
// delivery. It stores the message and, only once the bytes are actually
// owned by the channel (memory queue, backend queue or deferred heap),
// charges them against the channel and consumes the reserved fan-out share
// when the channel already existed at publish time.
func (c *Channel) deliverMessage(msg *Message) error {
	size := int64(len(msg.Body))

	if msg.deferred != 0 {
		err := c.StartDeferredTimeout(msg, msg.deferred)
		if err != nil {
			return err
		}
		c.topic.budgetMu.Lock()
		released := c.deliverLocked(msg, size)
		if released {
			c.topic.subReservedLocked(size)
		}
		c.topic.budgetMu.Unlock()
		atomic.AddUint64(&c.messageCount, 1)
		return nil
	}

	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()
	if c.Exiting() {
		return errors.New("exiting")
	}
	dropped, err := c.put(msg)
	if err != nil {
		return err
	}
	c.topic.budgetMu.Lock()
	if dropped {
		// ephemeral channel with a full queue: nothing was stored, give the
		// fan-out reservation back to the topic
		c.topic.releaseChannelReservationLocked(c, size)
	} else {
		released := c.deliverLocked(msg, size)
		if released {
			c.topic.subReservedLocked(size)
		}
	}
	c.topic.budgetMu.Unlock()
	atomic.AddUint64(&c.messageCount, 1)
	return nil
}

func (c *Channel) put(m *Message) (dropped bool, err error) {
	if c.topologyAwareConsumption {
		// Attempt zone local, region local and finally the memory channel
		// we do this to ensure that we preferentially deliver messages based on toplogy
		//
		// Because messagePump is intermittently unavailable while writing a msg to a client
		// we continue to add lower priority channels in the select loop, this means at each
		// attempt a higher priority channel can still win
		select {
		case c.zoneLocalMsgChan <- m:
			return false, nil
		default:
		}
		select {
		case c.zoneLocalMsgChan <- m:
			return false, nil
		case c.regionLocalMsgChan <- m:
			return false, nil
		default:
		}

		select {
		case c.zoneLocalMsgChan <- m:
			return false, nil
		case c.regionLocalMsgChan <- m:
			return false, nil
		case c.memoryMsgChan <- m:
			return false, nil
		default:
		}

	} else {

		select {
		case c.memoryMsgChan <- m:
			return false, nil
		default:
		}
	}

	if c.ephemeral {
		// ephemeral channels have no backend queue; a full memory queue drops
		// the message
		return true, nil
	}
	err = writeMessageToBackend(m, c.backend)
	c.nsqd.SetHealth(err)
	if err != nil {
		c.nsqd.logf(LOG_ERROR, "CHANNEL(%s): failed to write message to backend - %s",
			c.name, err)
		return false, err
	}
	return false, nil
}

func (c *Channel) PutMessageDeferred(msg *Message, timeout time.Duration) error {
	atomic.AddUint64(&c.messageCount, 1)
	return c.StartDeferredTimeout(msg, timeout)
}

// TouchMessage resets the timeout for an in-flight message
func (c *Channel) TouchMessage(clientID int64, id MessageID, clientMsgTimeout time.Duration) error {
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(msg)

	newTimeout := time.Now().Add(clientMsgTimeout)
	if newTimeout.Sub(msg.deliveryTS) >=
		c.nsqd.getOpts().MaxMsgTimeout {
		// we would have gone over, set to the max
		newTimeout = msg.deliveryTS.Add(c.nsqd.getOpts().MaxMsgTimeout)
	}

	msg.pri = newTimeout.UnixNano()
	err = c.pushInFlightMessage(msg)
	if err != nil {
		return err
	}
	c.addToInFlightPQ(msg)
	return nil
}

// FinishMessage successfully discards an in-flight message
func (c *Channel) FinishMessage(clientID int64, id MessageID) error {
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(msg)
	c.topic.budgetMu.Lock()
	c.subPendingLocked(int64(len(msg.Body)))
	c.topic.budgetMu.Unlock()
	if c.e2eProcessingLatencyStream != nil {
		c.e2eProcessingLatencyStream.Insert(msg.Timestamp)
	}
	return nil
}

// RequeueMessage requeues a message based on `time.Duration`, ie:
//
// `timeoutMs` == 0 - requeue a message immediately
// `timeoutMs`  > 0 - asynchronously wait for the specified timeout
//
//	and requeue a message (aka "deferred requeue")
func (c *Channel) RequeueMessage(clientID int64, id MessageID, timeout time.Duration) error {
	// remove from inflight first
	msg, err := c.popInFlightMessage(clientID, id)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(msg)
	atomic.AddUint64(&c.requeueCount, 1)

	if timeout == 0 {
		c.exitMutex.RLock()
		if c.Exiting() {
			c.exitMutex.RUnlock()
			return errors.New("exiting")
		}
		_, err = c.put(msg)
		c.exitMutex.RUnlock()
		return err
	}

	// deferred requeue
	return c.StartDeferredTimeout(msg, timeout)
}

// AddClient adds a client to the Channel's client list
func (c *Channel) AddClient(clientID int64, client Consumer) error {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return errors.New("exiting")
	}

	c.RLock()
	_, ok := c.clients[clientID]
	numClients := len(c.clients)
	c.RUnlock()
	if ok {
		return nil
	}

	maxChannelConsumers := c.nsqd.getOpts().MaxChannelConsumers
	if maxChannelConsumers != 0 && numClients >= maxChannelConsumers {
		return fmt.Errorf("consumers for %s:%s exceeds limit of %d",
			c.topicName, c.name, maxChannelConsumers)
	}

	c.Lock()
	c.clients[clientID] = client
	c.Unlock()
	return nil
}

// RemoveClient removes a client from the Channel's client list
func (c *Channel) RemoveClient(clientID int64) {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return
	}

	c.RLock()
	_, ok := c.clients[clientID]
	c.RUnlock()
	if !ok {
		return
	}

	c.Lock()
	delete(c.clients, clientID)
	numClients := len(c.clients)
	c.Unlock()

	if numClients == 0 && c.ephemeral {
		go c.deleter.Do(func() { c.deleteCallback(c) })
	}
}

func (c *Channel) StartInFlightTimeout(msg *Message, clientID int64, timeout time.Duration) error {
	now := time.Now()
	msg.clientID = clientID
	msg.deliveryTS = now
	msg.pri = now.Add(timeout).UnixNano()
	err := c.pushInFlightMessage(msg)
	if err != nil {
		return err
	}
	c.addToInFlightPQ(msg)
	return nil
}

func (c *Channel) StartDeferredTimeout(msg *Message, timeout time.Duration) error {
	absTs := time.Now().Add(timeout).UnixNano()
	item := &pqueue.Item{Value: msg, Priority: absTs}
	err := c.pushDeferredMessage(item)
	if err != nil {
		return err
	}
	c.addToDeferredPQ(item)
	return nil
}

// pushInFlightMessage atomically adds a message to the in-flight dictionary
func (c *Channel) pushInFlightMessage(msg *Message) error {
	c.inFlightMutex.Lock()
	_, ok := c.inFlightMessages[msg.ID]
	if ok {
		c.inFlightMutex.Unlock()
		return errors.New("ID already in flight")
	}
	c.inFlightMessages[msg.ID] = msg
	c.inFlightMutex.Unlock()
	return nil
}

// popInFlightMessage atomically removes a message from the in-flight dictionary
func (c *Channel) popInFlightMessage(clientID int64, id MessageID) (*Message, error) {
	c.inFlightMutex.Lock()
	msg, ok := c.inFlightMessages[id]
	if !ok {
		c.inFlightMutex.Unlock()
		return nil, errors.New("ID not in flight")
	}
	if msg.clientID != clientID {
		c.inFlightMutex.Unlock()
		return nil, errors.New("client does not own message")
	}
	delete(c.inFlightMessages, id)
	c.inFlightMutex.Unlock()
	return msg, nil
}

func (c *Channel) addToInFlightPQ(msg *Message) {
	c.inFlightMutex.Lock()
	c.inFlightPQ.Push(msg)
	c.inFlightMutex.Unlock()
}

func (c *Channel) removeFromInFlightPQ(msg *Message) {
	c.inFlightMutex.Lock()
	if msg.index == -1 {
		// this item has already been popped off the pqueue
		c.inFlightMutex.Unlock()
		return
	}
	c.inFlightPQ.Remove(msg.index)
	c.inFlightMutex.Unlock()
}

func (c *Channel) pushDeferredMessage(item *pqueue.Item) error {
	c.deferredMutex.Lock()
	// TODO: these map lookups are costly
	id := item.Value.(*Message).ID
	_, ok := c.deferredMessages[id]
	if ok {
		c.deferredMutex.Unlock()
		return errors.New("ID already deferred")
	}
	c.deferredMessages[id] = item
	c.deferredMutex.Unlock()
	return nil
}

func (c *Channel) popDeferredMessage(id MessageID) (*pqueue.Item, error) {
	c.deferredMutex.Lock()
	// TODO: these map lookups are costly
	item, ok := c.deferredMessages[id]
	if !ok {
		c.deferredMutex.Unlock()
		return nil, errors.New("ID not deferred")
	}
	delete(c.deferredMessages, id)
	c.deferredMutex.Unlock()
	return item, nil
}

func (c *Channel) addToDeferredPQ(item *pqueue.Item) {
	c.deferredMutex.Lock()
	heap.Push(&c.deferredPQ, item)
	c.deferredMutex.Unlock()
}

func (c *Channel) processDeferredQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	for {
		c.deferredMutex.Lock()
		item, _ := c.deferredPQ.PeekAndShift(t)
		c.deferredMutex.Unlock()

		if item == nil {
			goto exit
		}
		dirty = true

		msg := item.Value.(*Message)
		_, err := c.popDeferredMessage(msg.ID)
		if err != nil {
			goto exit
		}
		_, err = c.put(msg)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "CHANNEL(%s): failed to requeue deferred message - %s", c.name, err)
			goto exit
		}
	}

exit:
	return dirty
}

func (c *Channel) processInFlightQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	for {
		c.inFlightMutex.Lock()
		msg, _ := c.inFlightPQ.PeekAndShift(t)
		c.inFlightMutex.Unlock()

		if msg == nil {
			goto exit
		}
		dirty = true

		_, err := c.popInFlightMessage(msg.clientID, msg.ID)
		if err != nil {
			goto exit
		}
		atomic.AddUint64(&c.timeoutCount, 1)
		c.RLock()
		client, ok := c.clients[msg.clientID]
		c.RUnlock()
		if ok {
			client.TimedOutMessage()
		}
		_, err = c.put(msg)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "CHANNEL(%s): failed to requeue in-flight message - %s", c.name, err)
			goto exit
		}
	}

exit:
	return dirty
}

// subPendingLocked decreases the channel's pending byte counter, clamping at
// zero; the caller must hold the topic's budget mutex
func (c *Channel) subPendingLocked(n int64) {
	c.pendingBytes -= n
	if c.pendingBytes < 0 {
		c.pendingBytes = 0
	}
}

// reclaimLocked releases every byte still reserved for or pending in this
// channel (used when the channel is deleted); the caller must hold the
// topic's budget mutex. It returns the number of pending bytes (no longer
// counted against the topic's budget) and reserved bytes that are freed.
func (c *Channel) reclaimLocked() (int64, int64) {
	pending := c.pendingBytes
	reserved := c.reservedBytes
	c.pendingBytes = 0
	c.reservedBytes = 0
	return pending, reserved
}

// deliverLocked accounts for a single fan-out delivery of a message.
//
// If the channel already existed when the message was admitted (publishTS at
// or before channel creation), the reserved fan-out share is consumed;
// otherwise (channel created later, NSQ backlog semantics) the bytes are
// charged without consuming a reservation.
//
// It returns true if a reservation share was released. The caller must hold
// the topic's budget mutex.
func (c *Channel) deliverLocked(msg *Message, size int64) bool {
	c.pendingBytes += size
	if msg.publishTS != 0 && msg.publishTS >= c.createdAt && c.reservedBytes >= size {
		c.reservedBytes -= size
		return true
	}
	return false
}

// pendingUsage reports the channel's current used and reserved bytes; the
// caller must hold the topic's budget mutex
func (c *Channel) pendingUsage() (int64, int64) {
	return c.pendingBytes, c.reservedBytes
}
