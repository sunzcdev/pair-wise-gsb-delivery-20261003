-- Confirm due pending user leaves after the grace period.
--
-- This runs on a reaper tick. Every returned user genuinely has no live
-- connections: a concurrent reconnect removes the user from the pending zset
-- and increments the count before this script reads it, so it can never
-- confirm a leave for a user that came back.
--
-- KEYS[1] - per-user connection count hash key
-- KEYS[2] - per-user info hash key
-- KEYS[3] - pending user leave expire zset key
--
-- ARGV[1] - now in unix milliseconds
--
-- Returns: a flat array of {userID, infoPayload} pairs for confirmed leaves.

local due = redis.call("zrangebyscore", KEYS[3], "0", ARGV[1])
local result = {}

for i = 1, #due do
    local userID = due[i]
    local count = tonumber(redis.call("hget", KEYS[1], userID) or "0")
    if count <= 0 then
        local info = redis.call("hget", KEYS[2], userID)
        redis.call("hdel", KEYS[1], userID)
        redis.call("hdel", KEYS[2], userID)
        redis.call("zrem", KEYS[3], userID)
        result[#result + 1] = userID
        result[#result + 1] = info
    else
        -- Reconnected: drop the stale pending marker.
        redis.call("zrem", KEYS[3], userID)
    end
end

return result
