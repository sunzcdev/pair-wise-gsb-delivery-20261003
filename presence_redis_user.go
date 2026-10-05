package centrifuge

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/centrifugal/centrifuge/internal/convert"

	"github.com/centrifugal/protocol"
	"github.com/redis/rueidis"
)

var _ UserPresenceManager = (*RedisPresenceManager)(nil)

// userPresenceReapInterval is how often a node scans for due pending leaves
// and naturally-expired clients. The work is driven by Redis ZSET scores so
// scanning is cheap, and the interval only bounds event latency.
const userPresenceReapInterval = 100 * time.Millisecond

// AddUserPresence - see UserPresenceManager interface description.
func (m *RedisPresenceManager) AddUserPresence(ch string, clientID string, info *ClientInfo) (UserPresenceEvent, bool, error) {
	s := m.getShard(ch)
	m.registerChannel(context.Background(), s, ch)
	keys, args, err := m.userAddScriptKeysArgs(s, ch, clientID, info)
	if err != nil {
		return UserPresenceEvent{}, false, err
	}
	isEnter, err := m.userAddScript.Exec(context.Background(), s.client, keys, args).AsInt64()
	if err != nil {
		return UserPresenceEvent{}, false, err
	}
	if isEnter != 1 {
		return UserPresenceEvent{}, false, nil
	}
	return UserPresenceEvent{
		Channel: ch,
		Type:    UserPresenceEnter,
		UserID:  info.UserID,
		Info:    info,
	}, true, nil
}

// RemoveUserPresence - see UserPresenceManager interface description.
func (m *RedisPresenceManager) RemoveUserPresence(ch string, clientID string, userID string) (UserPresenceEvent, bool, error) {
	s := m.getShard(ch)
	keys, args, err := m.userRemScriptKeysArgs(s, ch, clientID, userID)
	if err != nil {
		return UserPresenceEvent{}, false, err
	}
	isLeave, err := m.userRemScript.Exec(context.Background(), s.client, keys, args).AsInt64()
	if err != nil {
		return UserPresenceEvent{}, false, err
	}
	if isLeave != 1 {
		return UserPresenceEvent{}, false, nil
	}
	return UserPresenceEvent{
		Channel: ch,
		Type:    UserPresenceLeave,
		UserID:  userID,
	}, true, nil
}

// SetUserPresenceEmitter wires deferred grace leaves and natural-expiry leaves
// to the Node cluster-wide fan-out and starts the background reaper once.
func (m *RedisPresenceManager) SetUserPresenceEmitter(emit func(UserPresenceEvent)) {
	m.userPresenceEmitterMu.Lock()
	m.userPresenceEmitter = emit
	m.userPresenceEmitterMu.Unlock()

	select {
	case <-m.stopCh:
		return
	default:
	}
	select {
	case <-m.userPresenceOnce:
		return
	default:
		close(m.userPresenceOnce)
	}
	go m.runUserPresenceReaper()
}

func (m *RedisPresenceManager) emitUserPresence(event UserPresenceEvent) {
	m.userPresenceEmitterMu.RLock()
	emit := m.userPresenceEmitter
	m.userPresenceEmitterMu.RUnlock()
	if emit != nil {
		emit(event)
	}
}

// userPresenceEffectiveTTLSeconds keeps the per-user structures alive for at
// least the presence TTL and through one full grace window (plus a margin for
// reaper scheduling), so a pending leave is never evicted before confirmation.
func (m *RedisPresenceManager) userPresenceEffectiveTTLSeconds() int {
	ttl := m.config.PresenceTTL
	grace := m.config.UserPresenceGracePeriod + 2*time.Second
	if grace > ttl {
		ttl = grace
	}
	if ttl < 1*time.Second {
		ttl = time.Second
	}
	return int(ttl.Seconds())
}

func (m *RedisPresenceManager) userAddScriptKeysArgs(s *RedisShard, ch string, clientID string, info *ClientInfo) ([]string, []string, error) {
	expire := int(m.config.PresenceTTL.Seconds())
	infoBytes, err := infoToProto(info).MarshalVT()
	if err != nil {
		return nil, nil, err
	}

	keys := []string{
		string(m.presenceHashKey(s, ch)),
		string(m.userExpireSetKey(s, ch)),
		string(m.userClientIndexKey(s, ch)),
		string(m.userCountHashKey(s, ch)),
		string(m.userInfoHashKey(s, ch)),
		string(m.userPendingSetKey(s, ch)),
	}

	expireAt := time.Now().Unix() + int64(expire)
	args := []string{
		strconv.Itoa(expire),
		strconv.FormatInt(expireAt, 10),
		clientID,
		convert.BytesToString(infoBytes),
		info.UserID,
		strconv.Itoa(m.userPresenceEffectiveTTLSeconds()),
	}
	return keys, args, nil
}

func (m *RedisPresenceManager) userRemScriptKeysArgs(s *RedisShard, ch string, clientID string, userID string) ([]string, []string, error) {
	keys := []string{
		string(m.presenceHashKey(s, ch)),
		string(m.userExpireSetKey(s, ch)),
		string(m.userClientIndexKey(s, ch)),
		string(m.userCountHashKey(s, ch)),
		string(m.userInfoHashKey(s, ch)),
		string(m.userPendingSetKey(s, ch)),
	}
	fireAt := time.Now().Add(m.config.UserPresenceGracePeriod).UnixMilli()
	args := []string{
		clientID,
		userID,
		strconv.FormatInt(fireAt, 10),
		strconv.FormatInt(m.config.UserPresenceGracePeriod.Milliseconds(), 10),
		strconv.Itoa(m.userPresenceEffectiveTTLSeconds()),
	}
	return keys, args, nil
}

// registerChannel records a channel in the Redis-backed active-channel
// registry so any node's reaper can reconcile it. The registry key uses a fixed
// hash slot in a Cluster, so the SADD is a separate command (the counting
// script itself stays atomic on the channel keys).
func (m *RedisPresenceManager) registerChannel(ctx context.Context, s *RedisShard, ch string) {
	cmd := s.client.B().Sadd().Key(string(m.activeChannelsKey())).Member(ch).Build()
	if err := s.client.Do(ctx, cmd).Error(); err != nil && m.node != nil {
		m.node.logger.log(newErrorLogEntry(err, "user presence: register channel", map[string]any{"channel": ch}))
	}
}

// runUserPresenceReaper periodically reconciles deferred transitions:
//
//   - client presence entries that expired naturally (node crash / network
//     partition), which must decrement user counts;
//   - pending user leaves whose grace period elapsed, which must be confirmed
//     unless a reconnect cancelled them.
//
// Both decisions are atomic inside Lua, so concurrent updates from other
// nodes are safe without read-then-decrement on the Go side.
func (m *RedisPresenceManager) runUserPresenceReaper() {
	ticker := time.NewTicker(userPresenceReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapUserPresence()
		}
	}
}

func (m *RedisPresenceManager) reapUserPresence() {
	ctx := context.Background()
	nowSec := strconv.FormatInt(time.Now().Unix(), 10)
	nowMillis := strconv.FormatInt(time.Now().UnixMilli(), 10)
	graceMillis := strconv.FormatInt(m.config.UserPresenceGracePeriod.Milliseconds(), 10)
	ttlSec := strconv.Itoa(m.userPresenceEffectiveTTLSeconds())

	seen := make(map[string]struct{})
	for _, s := range m.shards {
		for _, ch := range m.activeChannels(ctx, s) {
			if _, ok := seen[ch]; ok {
				continue
			}
			seen[ch] = struct{}{}
			m.reapChannel(ctx, s, ch, nowSec, nowMillis, graceMillis, ttlSec)
		}
	}
}

func (m *RedisPresenceManager) reapChannel(ctx context.Context, s *RedisShard, ch, nowSec, nowMillis, graceMillis, ttlSec string) {
	expireKeys := []string{
		string(m.presenceHashKey(s, ch)),
		string(m.userExpireSetKey(s, ch)),
		string(m.userClientIndexKey(s, ch)),
		string(m.userCountHashKey(s, ch)),
		string(m.userInfoHashKey(s, ch)),
		string(m.userPendingSetKey(s, ch)),
	}
	immediate, err := m.userExpireScript.Exec(ctx, s.client, expireKeys, []string{nowSec, nowMillis, graceMillis, ttlSec}).ToArray()
	if err != nil && !rueidis.IsRedisNil(err) {
		m.logReapError(err, ch, "expire clients")
		return
	}
	m.dispatchUserPairs(ch, immediate)

	confirmKeys := []string{
		string(m.userCountHashKey(s, ch)),
		string(m.userInfoHashKey(s, ch)),
		string(m.userPendingSetKey(s, ch)),
	}
	confirmed, err := m.userConfirmScript.Exec(ctx, s.client, confirmKeys, []string{nowMillis}).ToArray()
	if err != nil && !rueidis.IsRedisNil(err) {
		m.logReapError(err, ch, "confirm leaves")
		return
	}
	m.dispatchUserPairs(ch, confirmed)

	m.pruneIdleChannel(ctx, s, ch)
}

func (m *RedisPresenceManager) logReapError(err error, ch, action string) {
	if m.node != nil {
		m.node.logger.log(newErrorLogEntry(err, "user presence: "+action, map[string]any{"channel": ch}))
	}
}

// pruneIdleChannel drops a channel from the active registry once it holds no
// presence, no counts and no pending leaves. In a cluster the registry SREM
// must be a separate command (different slot); standalone setups prune inside
// the idle script instead.
func (m *RedisPresenceManager) pruneIdleChannel(ctx context.Context, s *RedisShard, ch string) {
	keys := []string{
		string(m.presenceHashKey(s, ch)),
		string(m.userCountHashKey(s, ch)),
		string(m.userPendingSetKey(s, ch)),
	}
	idle, err := m.userIdleScript.Exec(ctx, s.client, keys, nil).AsInt64()
	if err != nil && !rueidis.IsRedisNil(err) {
		return
	}
	if idle == 1 {
		cmd := s.client.B().Srem().Key(string(m.activeChannelsKey())).Member(ch).Build()
		_ = s.client.Do(ctx, cmd).Error()
	}
}

func (m *RedisPresenceManager) activeChannels(ctx context.Context, s *RedisShard) []string {
	cmd := s.client.B().Smembers().Key(string(m.activeChannelsKey())).Build()
	channels, err := s.client.Do(ctx, cmd).AsStrSlice()
	if err != nil {
		if m.node != nil {
			m.node.logger.log(newErrorLogEntry(err, "user presence: read active channels", nil))
		}
		return nil
	}
	return channels
}

func (m *RedisPresenceManager) dispatchUserPairs(ch string, pairs []rueidis.RedisMessage) {
	for i := 0; i+1 < len(pairs); i += 2 {
		userID, err := pairs[i].ToString()
		if err != nil {
			continue
		}
		var info *ClientInfo
		if payload, errPayload := pairs[i+1].ToString(); errPayload == nil && payload != "" {
			info = m.parseInfoPayload(payload)
		}
		if info == nil {
			info = &ClientInfo{UserID: userID}
		}
		m.emitUserPresence(UserPresenceEvent{
			Channel: ch,
			Type:    UserPresenceLeave,
			UserID:  userID,
			Info:    info,
		})
	}
}

func (m *RedisPresenceManager) parseInfoPayload(payload string) *ClientInfo {
	var f protocol.ClientInfo
	if err := f.UnmarshalVT(convert.StringToBytes(payload)); err != nil {
		return nil
	}
	return infoFromProto(&f)
}

func (m *RedisPresenceManager) userClientIndexKey(s *RedisShard, ch string) channelID {
	return m.wrapChannelKey(s, ".presence.user.clientidx.", ch)
}

// userExpireSetKey is a dedicated per-client expiry index for user presence.
// It is separate from the regular presence expire zset so the two expiry
// paths (presence reads pruning their own index, user-presence reaper pruning
// its own) never consume each other's expired entries and leak a user count.
func (m *RedisPresenceManager) userExpireSetKey(s *RedisShard, ch string) channelID {
	return m.wrapChannelKey(s, ".presence.user.expire.", ch)
}

func (m *RedisPresenceManager) userCountHashKey(s *RedisShard, ch string) channelID {
	return m.wrapChannelKey(s, ".presence.user.count.", ch)
}

func (m *RedisPresenceManager) userInfoHashKey(s *RedisShard, ch string) channelID {
	return m.wrapChannelKey(s, ".presence.user.info.", ch)
}

func (m *RedisPresenceManager) userPendingSetKey(s *RedisShard, ch string) channelID {
	return m.wrapChannelKey(s, ".presence.user.pending.", ch)
}

// activeChannelsKey is the Redis-backed registry of channels that currently
// have user-presence state. It uses a fixed hash slot in Cluster mode via a
// hash tag that does not collide with channel keys.
func (m *RedisPresenceManager) activeChannelsKey() channelID {
	return channelID(m.config.Prefix + ".presence.user.channels.{__userpresence__}")
}

func (m *RedisPresenceManager) wrapChannelKey(s *RedisShard, middle, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + middle + ch)
	}
	var builder strings.Builder
	builder.Grow(len(m.config.Prefix) + len(middle) + len(ch) + 2)
	builder.WriteString(m.config.Prefix)
	builder.WriteString(middle)
	builder.WriteByte('{')
	builder.WriteString(ch)
	builder.WriteByte('}')
	return channelID(builder.String())
}
