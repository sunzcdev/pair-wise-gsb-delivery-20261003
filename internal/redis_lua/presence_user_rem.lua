-- Atomically unregister a client connection for a (channel, user) pair and
-- report whether this removed the user's last live connection.
--
-- When the count reaches zero the user enters the grace period: the count is
-- kept at zero and the user is added to the pending-leave zset with the fire
-- timestamp. A concurrent presence_user_add cancels it atomically.
--
-- KEYS[1] - presence hash key
-- KEYS[2] - presence expire zset key
-- KEYS[3] - client -> user index hash key
-- KEYS[4] - per-user connection count hash key
-- KEYS[5] - per-user info hash key
-- KEYS[6] - pending user leave zset key
--
-- ARGV[1] - client ID
-- ARGV[2] - user ID
-- ARGV[3] - leave fire at (unix milliseconds)
-- ARGV[4] - grace period in milliseconds ("0" = leave immediately)
-- ARGV[5] - effective TTL seconds for user structures
--
-- Returns: 1 when this is an immediate user leave (1 -> 0), 0 otherwise.

local clientExisted = redis.call("hexists", KEYS[1], ARGV[1]) == 1

redis.call("hdel", KEYS[1], ARGV[1])
redis.call("hdel", KEYS[3], ARGV[1])
redis.call("zrem", KEYS[2], ARGV[1])

if not clientExisted then
    return 0
end

local count = tonumber(redis.call("hget", KEYS[4], ARGV[2]) or "0")
if count <= 0 then
    return 0
end

count = redis.call("hincrby", KEYS[4], ARGV[2], -1)

if count > 0 then
    return 0
end

if ARGV[4] == '0' then
    redis.call("hdel", KEYS[4], ARGV[2])
    redis.call("hdel", KEYS[5], ARGV[2])
    redis.call("zrem", KEYS[6], ARGV[2])
    return 1
end

-- Schedule the leave; keep count at zero and info around until either the
-- grace reaper confirms it or a reconnect cancels it.
redis.call("zadd", KEYS[6], ARGV[3], ARGV[2])
redis.call("expire", KEYS[4], ARGV[5])
redis.call("expire", KEYS[5], ARGV[5])
redis.call("expire", KEYS[6], ARGV[5])
return 0
