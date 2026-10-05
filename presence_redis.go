package centrifuge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/centrifugal/centrifuge/internal/convert"

	"github.com/centrifugal/protocol"
	"github.com/redis/rueidis"
)

var _ PresenceManager = (*RedisPresenceManager)(nil)

// RedisPresenceManager keeps presence in Redis thus allows scaling nodes.
type RedisPresenceManager struct {
	node                *Node
	config              RedisPresenceManagerConfig
	shards              []*RedisShard
	sharding            bool
	addPresenceScript   *rueidis.Lua
	remPresenceScript   *rueidis.Lua
	presenceScript      *rueidis.Lua
	presenceStatsScript *rueidis.Lua
	userAddScript       *rueidis.Lua
	userRemScript       *rueidis.Lua
	userConfirmScript   *rueidis.Lua
	userSweepScript     *rueidis.Lua

	eventsOnce    sync.Once
	closeCh       chan struct{}
	closeOnce     sync.Once
	pendingMu     sync.Mutex
	pendingLeaves map[string]map[string]*redisUserLeaveTimer
}

type redisUserLeaveTimer struct {
	token string
	timer *time.Timer
}

// RedisPresenceManagerConfig is a config for RedisPresenceManager.
type RedisPresenceManagerConfig struct {
	// Prefix to use before every channel name and key in Redis. By default,
	// "centrifuge" prefix will be used.
	Prefix string

	// PresenceTTL is an interval how long to consider presence info
	// valid after receiving presence update. This allows to automatically
	// clean up unnecessary presence entries after TTL passed. Zero value
	// means 60 seconds.
	PresenceTTL time.Duration

	// Shards is a slice of RedisShard to use. At least one shard must be provided.
	// Data will be consistently sharded by channel over provided Redis shards.
	Shards []*RedisShard

	// EnableUserMapping when returns true tells RedisPresenceManager to additionally store
	// user to num client connections hash map and sorted set with unique users in Redis.
	// This increases Redis memory usage since additional structures are used, but provides
	// a way to optimize presence stats retrieving as we can calculate stats quickly on
	// Redis side instead of loading the entire presence information. By default, user mapping
	// is not maintained.
	EnableUserMapping func(channel string) bool

	// UseHashFieldTTL allows using HEXPIRE command to set TTL for hash field. It's only available
	// since Redis 7.4.0 thus disabled by default. Using hash field TTL can be useful to avoid
	// maintaining expiration index in ZSET – so both useful from the throughput and memory usage
	// perspective.
	UseHashFieldTTL bool

	// ReadFromReplica enables reading presence information from replica Redis servers.
	// This only works in Redis Cluster and Sentinel setups and requires replica client
	// to be initialized in each RedisShard using RedisShardConfig.ReplicaClientEnabled.
	ReadFromReplica bool

	// LoadSHA1 enables loading SHA1 from Redis via SCRIPT LOAD instead of calculating
	// it on the client side. This is useful for FIPS compliance.
	LoadSHA1 bool

	// EnableUserPresenceEvents reports whether user-level presence events must be
	// maintained for a channel. When it returns true, the manager counts
	// connections per user in Redis (all decisions are atomic server-side, so
	// concurrent updates from two nodes are safe) and invokes the Node's
	// OnUserPresenceJoin / OnUserPresenceLeave handlers on zero-to-one and
	// one-to-zero transitions. Per-connection Join/Leave (EmitJoinLeave) is
	// independent and stays available regardless of this option.
	// By default user presence events are disabled.
	EnableUserPresenceEvents func(channel string) bool

	// UserLeaveGracePeriod is the interval to wait after a user's last connection
	// in a channel disappears before emitting the user leave event. A reconnect
	// from any node within this interval atomically cancels the pending leave and
	// no event is emitted. Zero means the leave fires as soon as the last
	// connection is gone. Only applies to channels for which
	// EnableUserPresenceEvents returns true.
	UserLeaveGracePeriod time.Duration

	// UserPresenceCleanupInterval is how often the background reconciler scans
	// presence for entries that expired naturally (never refreshed after an
	// abnormal loss) and drives the same user leave transitions. Zero means
	// PresenceTTL / 3. Set a negative value to disable the reconciler (not
	// recommended: natural expiry would then only be noticed lazily).
	UserPresenceCleanupInterval time.Duration
}

var (
	//go:embed internal/redis_lua/presence_add.lua
	addPresenceScriptSource string

	//go:embed internal/redis_lua/presence_rem.lua
	remPresenceScriptSource string

	//go:embed internal/redis_lua/presence_get.lua
	presenceScriptSource string

	//go:embed internal/redis_lua/presence_stats_get.lua
	presenceStatsScriptSource string

	//go:embed internal/redis_lua/presence_user_add.lua
	userAddPresenceScriptSource string

	//go:embed internal/redis_lua/presence_user_rem.lua
	userRemPresenceScriptSource string

	//go:embed internal/redis_lua/presence_user_confirm_leave.lua
	userConfirmLeaveScriptSource string

	//go:embed internal/redis_lua/presence_user_sweep.lua
	userSweepScriptSource string
)

// NewRedisPresenceManager creates new RedisPresenceManager.
func NewRedisPresenceManager(n *Node, config RedisPresenceManagerConfig) (*RedisPresenceManager, error) {
	if len(config.Shards) == 0 {
		return nil, errors.New("presence: no Redis shards provided in configuration")
	}

	if config.ReadFromReplica && !config.UseHashFieldTTL {
		return nil, errors.New("presence: ReadFromReplica requires UseHashFieldTTL to be enabled")
	}

	if config.ReadFromReplica {
		for i, s := range config.Shards {
			if s.replicaClient == nil {
				return nil, fmt.Errorf("presence: ReadFromReplica enabled but no replica client initialized in shard[%d] (ReplicaClientEnabled option)", i)
			}
		}
	}

	if len(config.Shards) > 1 {
		n.logger.log(newLogEntry(LogLevelInfo, fmt.Sprintf("presence: Redis sharding enabled: %d shards", len(config.Shards)), nil))
	}

	if config.Prefix == "" {
		config.Prefix = "centrifuge"
	}

	if config.PresenceTTL == 0 {
		config.PresenceTTL = 60 * time.Second
	}

	m := &RedisPresenceManager{
		node:     n,
		shards:   config.Shards,
		config:   config,
		sharding: len(config.Shards) > 1,

		addPresenceScript:   rueidis.NewLuaScript(addPresenceScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		remPresenceScript:   rueidis.NewLuaScript(remPresenceScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		presenceScript:      rueidis.NewLuaScript(presenceScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		presenceStatsScript: rueidis.NewLuaScript(presenceStatsScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		userAddScript:       rueidis.NewLuaScript(userAddPresenceScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		userRemScript:       rueidis.NewLuaScript(userRemPresenceScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		userConfirmScript:   rueidis.NewLuaScript(userConfirmLeaveScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		userSweepScript:     rueidis.NewLuaScript(userSweepScriptSource, rueidis.WithLoadSHA1(config.LoadSHA1)),
		closeCh:             make(chan struct{}),
		pendingLeaves:       make(map[string]map[string]*redisUserLeaveTimer),
	}
	return m, nil
}

func (m *RedisPresenceManager) Close(_ context.Context) error {
	m.closeOnce.Do(func() { close(m.closeCh) })
	m.pendingMu.Lock()
	for _, users := range m.pendingLeaves {
		for _, pt := range users {
			pt.timer.Stop()
		}
	}
	m.pendingLeaves = make(map[string]map[string]*redisUserLeaveTimer)
	m.pendingMu.Unlock()
	return nil
}

func (m *RedisPresenceManager) getShard(channel string) *RedisShard {
	if !m.sharding {
		return m.shards[0]
	}
	return m.shards[consistentIndex(channel, len(m.shards))]
}

// AddPresence - see PresenceManager interface description.
func (m *RedisPresenceManager) AddPresence(ch string, uid string, info *ClientInfo) error {
	s := m.getShard(ch)
	if m.userEventsEnabled(ch) {
		return m.addUserPresence(s, ch, uid, info)
	}
	return m.addPresence(s, ch, uid, info)
}

func (m *RedisPresenceManager) userEventsEnabled(ch string) bool {
	return m.config.EnableUserPresenceEvents != nil && m.config.EnableUserPresenceEvents(ch)
}

func (m *RedisPresenceManager) addPresenceScriptKeysArgs(s *RedisShard, ch string, uid string, info *ClientInfo) ([]string, []string, error) {
	expire := int(m.config.PresenceTTL.Seconds())
	infoBytes, err := infoToProto(info).MarshalVT()
	if err != nil {
		return nil, nil, err
	}

	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	userSetKey := m.userSetKey(s, ch)
	userHashKey := m.userHashKey(s, ch)
	keys := []string{string(setKey), string(hashKey), string(userSetKey), string(userHashKey)}

	expireAt := time.Now().Unix() + int64(expire)
	useUserMapping := m.useUserMappingArg(ch)
	args := []string{
		strconv.Itoa(expire), strconv.FormatInt(expireAt, 10), uid,
		convert.BytesToString(infoBytes), info.UserID, useUserMapping, m.useHashFieldTTLArg(),
	}

	return keys, args, nil
}

func (m *RedisPresenceManager) userClientMapKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + ".presence.user.events.clients." + ch)
	}
	return channelID(m.config.Prefix + ".presence.user.events.clients.{" + ch + "}")
}

// userActiveKey is the per-user expiry index (zset, userID -> latest unix
// second any of the user's connections refreshed presence). The sweeper uses it
// to find users whose *last* connection expired, independent of the presence
// zset which is indexed per client.
func (m *RedisPresenceManager) userActiveKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + ".presence.user.events.active." + ch)
	}
	return channelID(m.config.Prefix + ".presence.user.events.active.{" + ch + "}")
}

func (m *RedisPresenceManager) userCountKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + ".presence.user.events.roster." + ch)
	}
	return channelID(m.config.Prefix + ".presence.user.events.roster.{" + ch + "}")
}

func (m *RedisPresenceManager) userPendingKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + ".presence.user.events.pending." + ch)
	}
	return channelID(m.config.Prefix + ".presence.user.events.pending.{" + ch + "}")
}

func (m *RedisPresenceManager) userFireAtKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		return channelID(m.config.Prefix + ".presence.user.events.fireat." + ch)
	}
	return channelID(m.config.Prefix + ".presence.user.events.fireat.{" + ch + "}")
}

func (m *RedisPresenceManager) userEventChannel(s *RedisShard) string {
	idx := 0
	for i, sh := range m.shards {
		if sh == s {
			idx = i
			break
		}
	}
	return m.config.Prefix + ".presence.user.events." + strconv.Itoa(idx)
}

func (m *RedisPresenceManager) addUserPresence(s *RedisShard, ch string, uid string, info *ClientInfo) error {
	m.ensureEventsRuntime()

	expire := int(m.config.PresenceTTL.Seconds())
	infoBytes, err := infoToProto(info).MarshalVT()
	if err != nil {
		return err
	}

	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	userSetKey := m.userSetKey(s, ch)
	userHashKey := m.userHashKey(s, ch)
	clientMapKey := m.userClientMapKey(s, ch)
	activeKey := m.userActiveKey(s, ch)
	countKey := m.userCountKey(s, ch)
	pendingKey := m.userPendingKey(s, ch)
	fireAtKey := m.userFireAtKey(s, ch)

	keys := []string{
		string(setKey), string(hashKey), string(userSetKey), string(userHashKey),
		string(clientMapKey), string(activeKey), string(countKey), string(pendingKey), string(fireAtKey),
	}

	expireAt := time.Now().Unix() + int64(expire)
	useUserMapping := m.useUserMappingArg(ch)
	eventChannel := m.userEventChannel(s)

	args := []string{
		strconv.Itoa(expire),
		strconv.FormatInt(expireAt, 10),
		uid,
		convert.BytesToString(infoBytes),
		info.UserID,
		useUserMapping,
		m.useHashFieldTTLArg(),
		"1", // enable user presence events.
		eventChannel,
		strconv.Itoa(expire),
		ch,
	}

	resp := m.userAddScript.Exec(context.Background(), s.client, keys, args)
	if rueidis.IsRedisNil(resp.Error()) {
		return nil
	}
	if err := resp.Error(); err != nil {
		return err
	}
	joined, err := resp.AsInt64()
	if err != nil {
		return err
	}
	if joined == 1 {
		m.node.notifyUserPresenceJoin(UserPresenceJoinEvent{
			Channel:  ch,
			UserID:   info.UserID,
			ClientID: uid,
			Info:     info,
		})
	}
	return nil
}

func (m *RedisPresenceManager) useUserMappingArg(ch string) string {
	useUserMapping := "0"
	if m.config.EnableUserMapping != nil && m.config.EnableUserMapping(ch) {
		useUserMapping = "1"
	}
	return useUserMapping
}

func (m *RedisPresenceManager) useHashFieldTTLArg() string {
	useHashFieldTTL := "0"
	if m.config.UseHashFieldTTL {
		useHashFieldTTL = "1"
	}
	return useHashFieldTTL
}

func (m *RedisPresenceManager) addPresence(s *RedisShard, ch string, uid string, info *ClientInfo) error {
	keys, args, err := m.addPresenceScriptKeysArgs(s, ch, uid, info)
	if err != nil {
		return err
	}
	resp := m.addPresenceScript.Exec(context.Background(), s.client, keys, args)
	if rueidis.IsRedisNil(resp.Error()) {
		return nil
	}
	return resp.Error()
}

// RemovePresence - see PresenceManager interface description.
func (m *RedisPresenceManager) RemovePresence(ch string, clientID string, userID string) error {
	s := m.getShard(ch)
	if m.userEventsEnabled(ch) {
		return m.removeUserPresence(s, ch, clientID, userID)
	}
	return m.removePresence(s, ch, clientID, userID)
}

func (m *RedisPresenceManager) removePresenceScriptKeysArgs(s *RedisShard, ch string, uid string, userID string) ([]string, []string, error) {
	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	userSetKey := m.userSetKey(s, ch)
	userHashKey := m.userHashKey(s, ch)

	keys := []string{string(setKey), string(hashKey), string(userSetKey), string(userHashKey)}

	useUserMapping := m.useUserMappingArg(ch)
	args := []string{uid, userID, useUserMapping, m.useHashFieldTTLArg()}
	return keys, args, nil
}

func (m *RedisPresenceManager) removePresence(s *RedisShard, ch string, clientID string, userID string) error {
	keys, args, err := m.removePresenceScriptKeysArgs(s, ch, clientID, userID)
	if err != nil {
		return err
	}
	resp := m.remPresenceScript.Exec(context.Background(), s.client, keys, args)
	if rueidis.IsRedisNil(resp.Error()) {
		return nil
	}
	return resp.Error()
}

func (m *RedisPresenceManager) removeUserPresence(s *RedisShard, ch string, clientID string, userID string) error {
	m.ensureEventsRuntime()

	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	userSetKey := m.userSetKey(s, ch)
	userHashKey := m.userHashKey(s, ch)
	clientMapKey := m.userClientMapKey(s, ch)
	activeKey := m.userActiveKey(s, ch)
	countKey := m.userCountKey(s, ch)
	pendingKey := m.userPendingKey(s, ch)
	fireAtKey := m.userFireAtKey(s, ch)

	keys := []string{
		string(setKey), string(hashKey), string(userSetKey), string(userHashKey),
		string(clientMapKey), string(activeKey), string(countKey), string(pendingKey), string(fireAtKey),
	}

	pendingTTL := m.config.PresenceTTL + m.config.UserLeaveGracePeriod + time.Second
	args := []string{
		clientID,
		userID,
		m.useUserMappingArg(ch),
		m.useHashFieldTTLArg(),
		"1", // enable user presence events.
		m.userEventChannel(s),
		strconv.FormatInt(m.config.UserLeaveGracePeriod.Milliseconds(), 10),
		strconv.FormatInt(time.Now().UnixMilli(), 10),
		strconv.Itoa(int(pendingTTL.Seconds())),
		strconv.Itoa(int(m.config.PresenceTTL.Seconds())),
		ch,
	}

	resp := m.userRemScript.Exec(context.Background(), s.client, keys, args)
	if rueidis.IsRedisNil(resp.Error()) {
		return nil
	}
	if err := resp.Error(); err != nil {
		return err
	}
	leaveNow, err := resp.AsInt64()
	if err != nil {
		return err
	}
	if leaveNow == 1 {
		m.node.notifyUserPresenceLeave(UserPresenceLeaveEvent{Channel: ch, UserID: userID})
	}
	return nil
}

// Presence - see PresenceManager interface description.
func (m *RedisPresenceManager) Presence(ch string) (map[string]*ClientInfo, error) {
	return m.presence(m.getShard(ch), ch)
}

func (m *RedisPresenceManager) presenceScriptKeysArgs(s *RedisShard, ch string) ([]string, []string, error) {
	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	keys := []string{string(setKey), string(hashKey)}

	now := int(time.Now().Unix())
	args := []string{strconv.Itoa(now), m.useHashFieldTTLArg()}

	return keys, args, nil
}

func (m *RedisPresenceManager) presenceStatsScriptKeysArgs(s *RedisShard, ch string) ([]string, []string, error) {
	setKey := m.presenceSetKey(s, ch)
	hashKey := m.presenceHashKey(s, ch)
	userSetKey := m.userSetKey(s, ch)
	userHashKey := m.userHashKey(s, ch)
	keys := []string{string(setKey), string(hashKey), string(userSetKey), string(userHashKey)}

	now := int(time.Now().Unix())
	args := []string{strconv.Itoa(now), m.useHashFieldTTLArg()}

	return keys, args, nil
}

func (m *RedisPresenceManager) presence(s *RedisShard, ch string) (map[string]*ClientInfo, error) {
	keys, args, err := m.presenceScriptKeysArgs(s, ch)
	if err != nil {
		return nil, err
	}
	client := s.client
	if m.config.ReadFromReplica {
		client = s.replicaClient
	}
	resp, err := m.presenceScript.Exec(context.Background(), client, keys, args).ToArray()
	if err != nil {
		return nil, err
	}
	return mapStringClientInfo(resp)
}

func mapStringClientInfo(result []rueidis.RedisMessage) (map[string]*ClientInfo, error) {
	if len(result)%2 != 0 {
		return nil, errors.New("mapStringClientInfo expects even number of values result")
	}
	m := make(map[string]*ClientInfo, len(result)/2)
	for i := 0; i < len(result); i += 2 {
		key, err := result[i].ToString()
		if err != nil {
			return nil, errors.New("key is not string")
		}
		value, err := result[i+1].ToString()
		if err != nil {
			return nil, errors.New("value is not string")
		}
		var f protocol.ClientInfo
		err = f.UnmarshalVT(convert.StringToBytes(value))
		if err != nil {
			return nil, errors.New("can not unmarshal value to ClientInfo")
		}
		m[key] = infoFromProto(&f)
	}
	return m, nil
}

func (m *RedisPresenceManager) presenceStats(s *RedisShard, ch string) (PresenceStats, error) {
	keys, args, err := m.presenceStatsScriptKeysArgs(s, ch)
	if err != nil {
		return PresenceStats{}, err
	}
	client := s.client
	if m.config.ReadFromReplica {
		client = s.replicaClient
	}

	replies, err := m.presenceStatsScript.Exec(context.Background(), client, keys, args).ToArray()
	if err != nil {
		return PresenceStats{}, err
	}
	if len(replies) != 2 {
		return PresenceStats{}, errors.New("wrong Redis reply: must have two values")
	}
	numClients, err := replies[0].AsInt64()
	if err != nil {
		return PresenceStats{}, errors.New("wrong Redis reply num clients")
	}
	numUsers, err := replies[1].AsInt64()
	if err != nil {
		return PresenceStats{}, errors.New("wrong Redis reply num users")
	}

	return PresenceStats{
		NumClients: int(numClients),
		NumUsers:   int(numUsers),
	}, nil
}

// PresenceStats - see PresenceManager interface description.
func (m *RedisPresenceManager) PresenceStats(ch string) (PresenceStats, error) {
	if m.config.EnableUserMapping != nil && m.config.EnableUserMapping(ch) {
		return m.presenceStats(m.getShard(ch), ch)
	}

	presence, err := m.Presence(ch)
	if err != nil {
		return PresenceStats{}, err
	}

	numClients := len(presence)
	numUsers := 0
	uniqueUsers := map[string]struct{}{}

	for _, info := range presence {
		userID := info.UserID
		if _, ok := uniqueUsers[userID]; !ok {
			uniqueUsers[userID] = struct{}{}
			numUsers++
		}
	}

	return PresenceStats{
		NumClients: numClients,
		NumUsers:   numUsers,
	}, nil
}

func (m *RedisPresenceManager) presenceHashKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		// Fast path: simple concatenation is optimal for non-cluster
		return channelID(m.config.Prefix + ".presence.data." + ch)
	}

	// Cluster path: use builder to avoid multiple allocations
	var builder strings.Builder
	builder.Grow(len(m.config.Prefix) + 16 + 1 + len(ch) + 1)
	builder.WriteString(m.config.Prefix)
	builder.WriteString(".presence.data.{")
	builder.WriteString(ch)
	builder.WriteByte('}')
	return channelID(builder.String())
}

func (m *RedisPresenceManager) presenceSetKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		// Fast path: simple concatenation is optimal for non-cluster
		return channelID(m.config.Prefix + ".presence.expire." + ch)
	}

	// Cluster path: use builder to avoid multiple allocations
	var builder strings.Builder
	builder.Grow(len(m.config.Prefix) + 18 + 1 + len(ch) + 1)
	builder.WriteString(m.config.Prefix)
	builder.WriteString(".presence.expire.{")
	builder.WriteString(ch)
	builder.WriteByte('}')
	return channelID(builder.String())
}

func (m *RedisPresenceManager) userSetKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		// Fast path: simple concatenation is optimal for non-cluster
		return channelID(m.config.Prefix + ".presence.user.expire." + ch)
	}

	// Cluster path: use builder to avoid multiple allocations
	var builder strings.Builder
	builder.Grow(len(m.config.Prefix) + 23 + 1 + len(ch) + 1)
	builder.WriteString(m.config.Prefix)
	builder.WriteString(".presence.user.expire.{")
	builder.WriteString(ch)
	builder.WriteByte('}')
	return channelID(builder.String())
}

func (m *RedisPresenceManager) userHashKey(s *RedisShard, ch string) channelID {
	if !s.isCluster {
		// Fast path: simple concatenation is optimal for non-cluster
		return channelID(m.config.Prefix + ".presence.user.clients." + ch)
	}

	// Cluster path: use builder to avoid multiple allocations
	var builder strings.Builder
	builder.Grow(len(m.config.Prefix) + 24 + 1 + len(ch) + 1)
	builder.WriteString(m.config.Prefix)
	builder.WriteString(".presence.user.clients.{")
	builder.WriteString(ch)
	builder.WriteByte('}')
	return channelID(builder.String())
}
