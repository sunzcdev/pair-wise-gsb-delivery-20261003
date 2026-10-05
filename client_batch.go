package centrifuge

import (
	"fmt"
	"sync"
	"time"

	"github.com/centrifugal/protocol"
)

// BatchSubscribeRequest describes one channel of a Client.SubscribeBatch call.
type BatchSubscribeRequest struct {
	// Channel to subscribe the client to.
	Channel string
	// Options of the subscription, same as for Client.Subscribe.
	Options SubscribeOptions
}

// SubscribeBatch subscribes the client to several channels as one atomic group:
// either every subscription of the group is committed, or none of them is.
//
// Each channel goes through the same subscribe flow as Client.Subscribe –
// including history recovery, presence and join/leave – but the group shares
// one commit point: every subscribe push (with its recovered publications) is
// written first, then all subscriptions are committed under one lock, and only
// then the publications which came while the group was subscribing are
// released. So a real-time publication of an already-recovered channel can
// never overtake the success reply of the group, and if any channel fails the
// hub registrations, presence entries and recovery buffers of the others are
// rolled back.
//
// The per-client recovery buffer limit (ClientQueueMaxSize) is shared by all
// channels of the group, exactly as it is shared by the subscriptions made at
// connect time.
//
// A concurrent Unsubscribe of one of the channels waits for the group's commit
// point (same wait-gate as for Client.Subscribe), and a client which closes
// mid-batch rolls the whole group back. Duplicate channels in reqs, or a
// channel the client is already subscribed to, fail the whole batch before any
// work is done.
func (c *Client) SubscribeBatch(reqs []BatchSubscribeRequest) error {
	if len(reqs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(reqs))
	for _, req := range reqs {
		if req.Channel == "" {
			return fmt.Errorf("channel is empty")
		}
		if _, ok := seen[req.Channel]; ok {
			return fmt.Errorf("duplicate channel in batch: %s", req.Channel)
		}
		seen[req.Channel] = struct{}{}
	}

	channelLimit := c.node.config.ClientChannelLimit

	// Reserve all channels of the group under one lock, so that no concurrent
	// subscribe can slip between the reservations of the group. The reservation
	// carries a subscribingCh (with flags==0, so flagServerSide is not yet set),
	// which the unsubscribe wait-gate uses to synchronize a racing Unsubscribe –
	// the same contract Client.Subscribe establishes for a single channel.
	c.mu.Lock()
	if c.status == statusClosed {
		c.mu.Unlock()
		return nil
	}
	numChannels := len(c.channels) + len(c.mapSubscribing)
	if channelLimit > 0 && numChannels+len(reqs) > channelLimit {
		c.mu.Unlock()
		go func() { _ = c.close(DisconnectChannelLimit) }()
		return nil
	}
	for _, req := range reqs {
		if _, ok := c.channels[req.Channel]; ok {
			c.mu.Unlock()
			return ErrorAlreadySubscribed
		}
		// Also reject while a map subscribe is loading this channel (mirrors
		// Client.Subscribe): it reserves c.mapSubscribing rather than c.channels.
		if _, ok := c.mapSubscribing[req.Channel]; ok {
			c.mu.Unlock()
			return ErrorAlreadySubscribed
		}
	}
	reservedGens := make(map[string]uint64, len(reqs))
	for _, req := range reqs {
		gen := c.subGenCounter.Add(1)
		reservedGens[req.Channel] = gen
		c.channels[req.Channel] = ChannelContext{subscribingCh: make(chan struct{}), subGen: gen}
	}
	c.mu.Unlock()

	// Subscribe the channels concurrently, as connect-time server-side
	// subscriptions do: each subscribeCmd buffers its channel's publications
	// from before its hub registration until its result is written (see
	// recovery.PubSubSync), so a channel which finished its recovery holds the
	// publications it receives while the rest of the group is still subscribing.
	subCtxMap := make(map[string]subscribeContext, len(reqs))
	var subMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(reqs))
	for _, req := range reqs {
		go func(req BatchSubscribeRequest) {
			defer wg.Done()
			subCmd := &protocol.SubscribeRequest{
				Channel: req.Channel,
			}
			if req.Options.RecoverSince != nil {
				subCmd.Recover = true
				subCmd.Offset = req.Options.RecoverSince.Offset
				subCmd.Epoch = req.Options.RecoverSince.Epoch
			}
			subCtx := c.subscribeCmd(subCmd, SubscribeReply{
				Options: req.Options,
			}, nil, true, time.Time{}, nil)
			subMu.Lock()
			subCtxMap[req.Channel] = subCtx
			subMu.Unlock()
		}(req)
	}
	wg.Wait()

	// All-or-nothing: one failed channel fails the group. The failed channel
	// undid its own presence and buffer on the way out of subscribeCmd; the
	// rollback below undoes the reservations, hub registrations and presence
	// entries of the successful ones, and cancels their buffers.
	var batchErr error
	for _, req := range reqs {
		subCtx := subCtxMap[req.Channel]
		if subCtx.disconnect != nil {
			batchErr = subCtx.disconnect
			break
		}
		if subCtx.err != nil {
			batchErr = subCtx.err
		}
	}
	if batchErr != nil {
		c.cancelServerSideBuffering(subCtxMap)
		c.rollbackServerSideSubscribes(subCtxMap, reservedGens)
		return batchErr
	}

	// Drops the publications buffered for the group on the paths which return
	// before the buffers are stopped below. Does nothing after that.
	defer c.cancelServerSideBuffering(subCtxMap)

	// Every subscribe push of the group (with its recovered publications) is
	// written before the commit, as the reply of a client-side subscribe is: an
	// unsubscribe waits for the group until the commit, so its push can't come
	// before them. The publications queued since each channel's sync point stay
	// buffered until after the commit – nothing of the group overtakes its
	// success. A write error means the client is closing, the commit rolls back
	// then.
	subscribePushDisabled := hasFlag(c.transport.DisabledPushFlags(), PushFlagSubscribe)
	var writeErr error
	for _, req := range reqs {
		if err := c.writeSubscribePush(req.Channel, subCtxMap[req.Channel].result, subscribePushDisabled); err != nil && writeErr == nil {
			writeErr = err
		}
	}

	// The commit point of the group: under one lock either every reservation is
	// replaced with its live channel context, or – the client closed or a
	// reservation was lost to the unsubscribe wait timeout – none is, and every
	// hub registration and presence entry of the group is rolled back.
	subscribingChs, committed := c.commitBatchSubscriptions(subCtxMap, reservedGens)
	if !committed {
		return DisconnectServerError
	}
	for _, subscribingCh := range subscribingChs {
		close(subscribingCh)
	}
	if writeErr != nil {
		return writeErr
	}
	// The group is committed and its results are written, so the publications
	// queued since each channel's sync point can follow them.
	for _, req := range reqs {
		if c.pubSubSync.StopBuffering(subCtxMap[req.Channel].pubSubBuffer, c.writePendingPublication) {
			go c.handleInsufficientState(req.Channel, true)
		}
	}
	if subscribePushDisabled {
		return nil
	}
	for _, req := range reqs {
		subCtx := subCtxMap[req.Channel]
		if subCtx.clientInfo != nil {
			// Synchronous: see the comment on the same call in handleSubscribe.
			c.publishJoinAndPresence(req.Channel, subCtx.channelContext, subCtx.clientInfo)
		}
	}
	return nil
}

// commitBatchSubscriptions is the commit point of SubscribeBatch, the group
// analogue of commitSubscription. Under c.mu it either installs every finished
// subscription's channel context into c.channels, or – if the client closed
// while the batch was in flight, or a reservation was lost to the unsubscribe
// wait timeout – rolls the whole group back.
//
// On success it returns the reservations' subscribingChs (or nils) and true.
// The caller closes them to release any unsubscribe waiting on an in-flight
// subscribe of the group.
//
// On rollback it drops the reservations the batch still owns (identity-matched
// on subGen, so a fresh subscribe which took over a lost reservation is left
// alone), removes the hub entries and presence entries the batch registered,
// closes the owned subscribingChs – in that order, so a woken waiter never
// observes a half-rolled-back group – and returns (nil, false). The caller
// cancels the recovery buffers. Lock must NOT be held.
func (c *Client) commitBatchSubscriptions(subCtxMap map[string]subscribeContext, reservedGens map[string]uint64) ([]chan struct{}, bool) {
	c.mu.Lock()
	committed := c.status != statusClosed
	var subscribingChs []chan struct{}
	for channel := range subCtxMap {
		resv, ok := c.channels[channel]
		if !ok || resv.subGen != reservedGens[channel] {
			// The entry (if any) belongs to a different subscription attempt –
			// do not touch it or its subscribingCh, and do not commit the rest
			// of the group either: it commits whole or not at all.
			committed = false
			continue
		}
		if resv.subscribingCh != nil {
			subscribingChs = append(subscribingChs, resv.subscribingCh)
		}
	}
	if !committed {
		for channel := range subCtxMap {
			if resv, ok := c.channels[channel]; ok && resv.subGen == reservedGens[channel] {
				delete(c.channels, channel)
			}
		}
		c.mu.Unlock()
		for channel, subCtx := range subCtxMap {
			// Gen-matched, so a no-op for a reservation the batch lost.
			_ = c.node.removeSubscription(channel, c, reservedGens[channel])
			// Presence was added before the commit; close() snapshotted
			// c.channels before these channels entered it, so it can't remove
			// the entries – remove them here or they linger until PresenceTTL.
			c.removeSubscribePresence(channel, subCtx.channelContext.flags)
		}
		// Wake any waiter only after the rollback is complete (same ordering as
		// commitSubscription's closed path): the waiter is typically the
		// unsubscribe loop inside close(), and close() fires OnDisconnect right
		// after it.
		for _, subscribingCh := range subscribingChs {
			close(subscribingCh)
		}
		return nil, false
	}
	for channel, subCtx := range subCtxMap {
		ctx := subCtx.channelContext
		ctx.subscribingCh = nil
		c.channels[channel] = ctx
	}
	c.mu.Unlock()
	return subscribingChs, true
}
