-- Atomically register a client connection for a (channel, user) pair and
-- report whether this is a user-level enter transition.
--
-- This script always maintains its own per-client expiry zset and client->user
-- index (independent of UseHashFieldTTL) so natural TTL expiry can be
-- reconciled into user counts with identical semantics on every deployment.
--
-- KEYS[1] - presence hash key (client ID -> info payload)
-- KEYS[2] - presence expire zset key (client ID -> expire at, seconds)
-- KEYS[3] - client -> user index hash key
-- KEYS[4] - per-user connection count hash key
-- KEYS[5] - per-user info hash key
-- KEYS[6] - pending user leave zset key (user ID -> fire at, milliseconds)
--
-- ARGV[1] - presence TTL seconds
-- ARGV[2] - presence expire at (unix seconds)
-- ARGV[3] - client ID
-- ARGV[4] - info payload
-- ARGV[5] - user ID
-- ARGV[6] - effective TTL seconds for user structures
--
-- Returns: 1 when this is a user enter (0 -> 1), 0 otherwise.

redis.call("hset", KEYS[1], ARGV[3], ARGV[4])
redis.call("zadd", KEYS[2], ARGV[2], ARGV[3])
redis.call("expire", KEYS[1], ARGV[1])
redis.call("expire", KEYS[2], ARGV[1])

redis.call("hset", KEYS[3], ARGV[3], ARGV[5])
redis.call("expire", KEYS[3], ARGV[1])

local isEnter = 0
local count = tonumber(redis.call("hget", KEYS[4], ARGV[5]) or "0")

-- A pending leave for this user, if any, is cancelled by this reconnect.
redis.call("zrem", KEYS[6], ARGV[5])

if count <= 0 then
    isEnter = 1
    redis.call("hset", KEYS[5], ARGV[5], ARGV[4])
end

redis.call("hincrby", KEYS[4], ARGV[5], 1)
redis.call("expire", KEYS[4], ARGV[6])
redis.call("expire", KEYS[5], ARGV[6])
redis.call("expire", KEYS[6], ARGV[6])

return isEnter
