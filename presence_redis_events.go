package centrifuge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/rueidis"
)

// userPresencePubSubMessage is the payload exchanged between nodes over the
// per-shard user presence event channel. It is produced from Lua (cjson), so
// the JSON tags must stay in sync with the scripts.
type userPresencePubSubMessage struct {
	Type string `json:"t"`
	Ch   string `json:"c"`
	User string `json:"u"`
	// FireAtMs is the absolute unix millisecond at which the leave should fire.
	FireAtMs int64 `json:"f"`
	// Token uniquely identifies a pending leave and guards the confirm call.
	Token string `json:"k"`
}

// ensureEventsRuntime lazily starts the per-shard PUB/SUB receivers and the
// natural-expiry reconciler once user presence events are actually used.
func (m *RedisPresenceManager) ensureEventsRuntime() {
	m.eventsOnce.Do(func() {
		for i := range m.shards {
			go m.runEventReceiver(m.shards[i], i)
		}
		go m.runSweepLoop()
	})
}

func (m *RedisPresenceManager) runEventReceiver(s *RedisShard, shardIdx int) {
	channel := m.userEventChannel(s)

	for {
		select {
		case <-m.closeCh:
			return
		default:
		}

		err := m.receiveEvents(s, channel)
		if err != nil {
			m.node.logger.log(newErrorLogEntry(err, "presence: user presence event subscription stopped", map[string]any{
				"shard": shardIdx,
			}))
		}

		select {
		case <-m.closeCh:
			return
		case <-time.After(time.Second):
		}
	}
}

func (m *RedisPresenceManager) receiveEvents(s *RedisShard, channel string) error {
	conn, cancel := s.client.Dedicate()
	defer cancel()
	defer conn.Close()

	wait := conn.SetPubSubHooks(rueidis.PubSubHooks{
		OnMessage: func(msg rueidis.PubSubMessage) {
			if msg.Channel != channel {
				return
			}
			var payload userPresencePubSubMessage
			if err := json.Unmarshal([]byte(msg.Message), &payload); err != nil {
				m.node.logger.log(newErrorLogEntry(err, "presence: bad user presence event", nil))
				return
			}
			m.handleEventMessage(s, payload)
		},
	})

	ctx := context.Background()
	if err := conn.Do(ctx, conn.B().Subscribe().Channel(channel).Build()).Error(); err != nil {
		return err
	}

	select {
	case <-m.closeCh:
		return nil
	case <-wait:
		return nil
	}
}

func (m *RedisPresenceManager) handleEventMessage(s *RedisShard, msg userPresencePubSubMessage) {
	switch msg.Type {
	case "arm":
		m.armLeaveTimer(s, msg)
	case "cancel":
		m.cancelLeaveTimer(msg.Ch, msg.User, msg.Token)
	}
}

// armLeaveTimer schedules the local confirm+emit for a pending leave. The delay
// is derived from the shared absolute FireAtMs, so every node armed by the same
// message reaches its deadline at the same instant; only the node whose confirm
// wins emits. Duplicate arms for the same token are de-duplicated locally.
func (m *RedisPresenceManager) armLeaveTimer(s *RedisShard, msg userPresencePubSubMessage) {
	key := msg.Ch + "|" + msg.User

	m.pendingMu.Lock()
	users := m.pendingLeaves[key]
	if users == nil {
		users = make(map[string]*redisUserLeaveTimer)
		m.pendingLeaves[key] = users
	}
	if existing, ok := users[msg.Token]; ok {
		m.pendingMu.Unlock()
		_ = existing
		return
	}

	delay := time.Until(time.UnixMilli(msg.FireAtMs))
	if delay < 0 {
		delay = 0
	}

	timer := time.AfterFunc(delay, func() {
		m.confirmLeave(s, msg)
	})
	users[msg.Token] = &redisUserLeaveTimer{token: msg.Token, timer: timer}
	m.pendingMu.Unlock()
}

// cancelLeaveTimer stops a pending leave scheduled on this node when a
// reconnect cancels it cluster-wide.
func (m *RedisPresenceManager) cancelLeaveTimer(ch, userID, token string) {
	key := ch + "|" + userID
	m.pendingMu.Lock()
	if users := m.pendingLeaves[key]; users != nil {
		if pt, ok := users[token]; ok {
			pt.timer.Stop()
			delete(users, token)
			if len(users) == 0 {
				delete(m.pendingLeaves, key)
			}
		}
	}
	m.pendingMu.Unlock()
}

// removeLeaveTimer drops the local timer entry after a confirm attempt.
func (m *RedisPresenceManager) removeLeaveTimer(ch, userID, token string) {
	key := ch + "|" + userID
	m.pendingMu.Lock()
	if users := m.pendingLeaves[key]; users != nil {
		if pt, ok := users[token]; ok {
			pt.timer.Stop()
		}
		delete(users, token)
		if len(users) == 0 {
			delete(m.pendingLeaves, key)
		}
	}
	m.pendingMu.Unlock()
}

// confirmLeave atomically claims the pending leave in Redis. Only the node that
// wins emits the event; a reconnect's cancel removes the record first, so the
// confirm then loses and nothing is emitted.
func (m *RedisPresenceManager) confirmLeave(s *RedisShard, msg userPresencePubSubMessage) {
	defer m.removeLeaveTimer(msg.Ch, msg.User, msg.Token)

	keys := []string{string(m.userPendingKey(s, msg.Ch)), string(m.userFireAtKey(s, msg.Ch))}
	args := []string{msg.User, msg.Token}

	resp := m.userConfirmScript.Exec(context.Background(), s.client, keys, args)
	if rueidis.IsRedisNil(resp.Error()) {
		return
	}
	if err := resp.Error(); err != nil {
		m.node.logger.log(newErrorLogEntry(err, "presence: failed to confirm user leave", map[string]any{
			"channel": msg.Ch, "user": msg.User,
		}))
		return
	}
	won, err := resp.AsInt64()
	if err != nil {
		return
	}
	if won == 1 {
		m.node.notifyUserPresenceLeave(UserPresenceLeaveEvent{Channel: msg.Ch, UserID: msg.User})
	}
}

// runSweepLoop periodically reconciles naturally expired presence entries on
// every shard. The sweep script performs the same atomic zero-transition logic
// as an explicit remove; immediate leaves (grace zero) are returned directly,
// otherwise an arm message is published and handled via PUB/SUB.
func (m *RedisPresenceManager) runSweepLoop() {
	interval := m.config.UserPresenceCleanupInterval
	if interval == 0 {
		interval = m.config.PresenceTTL / 3
	}
	if interval < 0 {
		return
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.closeCh:
			return
		case <-ticker.C:
			m.sweepShards()
		}
	}
}

func (m *RedisPresenceManager) sweepShards() {
	var wg sync.WaitGroup
	for _, s := range m.shards {
		wg.Add(1)
		go func(s *RedisShard) {
			defer wg.Done()
			m.sweepShard(s)
		}(s)
	}
	wg.Wait()
}

// activeUserPresenceChannels returns the channels on this shard that currently
// carry a per-user presence structure. This is a scan over the dedicated
// count-key prefix; the scan is best-effort (a key created concurrently is
// swept on the next cycle).
func (m *RedisPresenceManager) activeUserPresenceChannels(ctx context.Context, s *RedisShard) []string {
	// Match the roster keys built in presence_redis.go: prefix.roster.channel or
	// prefix.roster.{channel} in cluster mode.
	plainPrefix := m.config.Prefix + ".presence.user.events.roster."
	pattern := plainPrefix + "*"
	stripPrefix := plainPrefix
	if s.isCluster {
		stripPrefix = m.config.Prefix + ".presence.user.events.roster.{"
		pattern = stripPrefix + "*"
	}

	var channels []string
	seen := make(map[string]struct{})

	// SCAN is node-local; Nodes() covers every primary (and, on a single client,
	// just itself), so iterate all of them.
	for _, nodeClient := range s.client.Nodes() {
		var cursor uint64
		for {
			resp, err := nodeClient.Do(ctx, nodeClient.B().Scan().Cursor(cursor).Match(pattern).Count(200).Build()).AsScanEntry()
			if err != nil {
				break
			}
			cursor = resp.Cursor
			for _, key := range resp.Elements {
				ch, ok := channelFromUserPresenceKey(key, stripPrefix, s.isCluster)
				if !ok {
					continue
				}
				if _, dup := seen[ch]; dup {
					continue
				}
				seen[ch] = struct{}{}
				channels = append(channels, ch)
			}
			if cursor == 0 {
				break
			}
		}
	}
	return channels
}

func channelFromUserPresenceKey(key, prefix string, isCluster bool) (string, bool) {
	if len(key) <= len(prefix) {
		return "", false
	}
	rest := key[len(prefix):]
	if isCluster {
		// Keys are prefix{channel}; strip the wrapping hash-tag braces.
		if len(rest) >= 1 && rest[0] == '{' {
			rest = rest[1:]
		}
		if idx := strings.LastIndexByte(rest, '}'); idx >= 0 {
			rest = rest[:idx]
		}
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

func (m *RedisPresenceManager) sweepShard(s *RedisShard) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	channels := m.activeUserPresenceChannels(ctx, s)
	for _, ch := range channels {
		select {
		case <-m.closeCh:
			return
		default:
		}
		if !m.userEventsEnabled(ch) {
			continue
		}
		m.sweepChannel(ctx, s, ch)
	}
}

func (m *RedisPresenceManager) sweepChannel(ctx context.Context, s *RedisShard, ch string) {
	keys := []string{
		string(m.presenceSetKey(s, ch)),
		string(m.presenceHashKey(s, ch)),
		string(m.userSetKey(s, ch)),
		string(m.userHashKey(s, ch)),
		string(m.userClientMapKey(s, ch)),
		string(m.userActiveKey(s, ch)),
		string(m.userCountKey(s, ch)),
		string(m.userPendingKey(s, ch)),
		string(m.userFireAtKey(s, ch)),
	}

	pendingTTL := m.config.PresenceTTL + m.config.UserLeaveGracePeriod + time.Second
	args := []string{
		strconv.FormatInt(time.Now().Unix(), 10),
		strconv.FormatInt(time.Now().UnixMilli(), 10),
		strconv.FormatInt(m.config.UserLeaveGracePeriod.Milliseconds(), 10),
		m.userEventChannel(s),
		ch,
		"200", // batch size.
		strconv.Itoa(int(pendingTTL.Seconds())),
		strconv.Itoa(int(m.config.PresenceTTL.Seconds())),
		m.useUserMappingArg(ch),
	}

	replies, err := m.userSweepScript.Exec(ctx, s.client, keys, args).ToArray()
	if err != nil {
		m.node.logger.log(newErrorLogEntry(err, "presence: user presence sweep failed", map[string]any{
			"channel": ch,
		}))
		return
	}

	// Response is [immediate leaves..., "---", stale arm tokens...].
	immediate := true
	for _, reply := range replies {
		val, err := reply.ToString()
		if err != nil {
			continue
		}
		if val == "---" {
			immediate = false
			continue
		}
		if immediate {
			m.node.notifyUserPresenceLeave(UserPresenceLeaveEvent{Channel: ch, UserID: val})
		}
		// Stale arms are republished by the script and arrive via PUB/SUB, so no
		// local action is needed for the token half.
	}
}
