-- Reconcile naturally-expired client connections into per-user counts.
--
-- Client presence entries can outlive an explicit removal (a node that
-- crashed never calls it). This script removes every expired client and
-- decrements its owner's user count by the number of expired connections.
-- Users whose last connection expired enter the grace period exactly like an
-- explicit removal; with no grace period they are returned for immediate
-- delivery.
--
-- KEYS[1] - presence hash key
-- KEYS[2] - presence expire zset key
-- KEYS[3] - client -> user index hash key
-- KEYS[4] - per-user connection count hash key
-- KEYS[5] - per-user info hash key
-- KEYS[6] - pending user leave zset key
--
-- ARGV[1] - now in unix seconds
-- ARGV[2] - leave fire at (unix milliseconds)
-- ARGV[3] - grace period in milliseconds ("0" = leave immediately)
-- ARGV[4] - effective TTL seconds for user structures
--
-- Returns: a flat array of {userID, infoPayload} pairs for immediate leaves.
-- Grace-deferred leaves are delivered via presence_user_confirm.

local expiredClients = redis.call("zrangebyscore", KEYS[2], "0", ARGV[1])
if #expiredClients == 0 then
    return {}
end

local expiredPerUser = {}
local userOrder = {}

for i = 1, #expiredClients do
    local clientID = expiredClients[i]
    local userID = redis.call("hget", KEYS[3], clientID)

    redis.call("hdel", KEYS[1], clientID)
    redis.call("hdel", KEYS[3], clientID)
    redis.call("zrem", KEYS[2], clientID)

    if userID then
        if expiredPerUser[userID] == nil then
            expiredPerUser[userID] = 0
            userOrder[#userOrder + 1] = userID
        end
        expiredPerUser[userID] = expiredPerUser[userID] + 1
    end
end

local immediateLeaves = {}

for i = 1, #userOrder do
    local userID = userOrder[i]
    local count = tonumber(redis.call("hget", KEYS[4], userID) or "0")
    local remaining = count - expiredPerUser[userID]
    if remaining < 0 then
        remaining = 0
    end

    if remaining > 0 then
        redis.call("hincrby", KEYS[4], userID, -expiredPerUser[userID])
    else
        redis.call("hset", KEYS[4], userID, 0)
        local info = redis.call("hget", KEYS[5], userID)
        if ARGV[3] == '0' then
            redis.call("hdel", KEYS[4], userID)
            redis.call("hdel", KEYS[5], userID)
            redis.call("zrem", KEYS[6], userID)
            immediateLeaves[#immediateLeaves + 1] = userID
            immediateLeaves[#immediateLeaves + 1] = info
        else
            redis.call("zadd", KEYS[6], ARGV[2], userID)
            redis.call("expire", KEYS[4], ARGV[4])
            redis.call("expire", KEYS[5], ARGV[4])
            redis.call("expire", KEYS[6], ARGV[4])
        end
    end
end

return immediateLeaves
