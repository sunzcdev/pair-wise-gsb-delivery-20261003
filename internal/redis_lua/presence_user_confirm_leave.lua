-- Confirm a pending user leave after the grace period elapsed.
--
-- Exactly one node wins this call (the one that atomically deletes the pending
-- record with a token comparison). A reconnect during the grace period removed
-- the record and published a cancel, so this then returns 0 and the leave is not
-- emitted. A stale or duplicate timer for the same user likewise loses.
--
-- KEYS[1] - user presence pending leave hash key
-- KEYS[2] - user presence fire-at zset key
--
-- ARGV[1] - user ID
-- ARGV[2] - token
--
-- Returns 1 when the caller owns the leave and must emit it, 0 otherwise.

local record = redis.call("hget", KEYS[1], ARGV[1])
if not record then
    return 0
end

local token = string.match(record, "^(.*)|[^|]*$") or record
if token ~= ARGV[2] then
    return 0
end

redis.call("hdel", KEYS[1], ARGV[1])
redis.call("zrem", KEYS[2], ARGV[1])
return 1
