-- Reconcile per-user presence for one channel after natural expiry.
--
-- Runs periodically and performs the same zero-transition decision as an
-- explicit remove, so natural expiry, abnormal disconnect and unsubscribe share
-- one counting semantic. Concurrent sweeps on two nodes cannot both fire: all
-- mutations happen in this single atomic script.
--
-- The active-users zset (KEYS[6]) holds the latest expiry score across each
-- user's clients. Users past it have no refreshed connection; their roster
-- (KEYS[7]) lists the stale client IDs to clean.
--
-- KEYS[1] - presence zset key (client expire index)
-- KEYS[2] - presence hash key
-- KEYS[3] - per-user stats zset key
-- KEYS[4] - per-user stats hash key
-- KEYS[5] - user events client map hash key (clientID -> userID)
-- KEYS[6] - user events active users zset key
-- KEYS[7] - user events roster hash key (userID -> CSV client IDs)
-- KEYS[8] - user events pending leave hash key
-- KEYS[9] - user events pending fire-at zset key
--
-- ARGV[1]  - current unix seconds
-- ARGV[2]  - current unix milliseconds
-- ARGV[3]  - leave grace period milliseconds ("0" = immediate)
-- ARGV[4]  - PUB/SUB channel
-- ARGV[5]  - channel name
-- ARGV[6]  - max expired users per run
-- ARGV[7]  - pending leave TTL seconds
-- ARGV[8]  - presence TTL seconds (key keep-alive)
-- ARGV[9]  - enable stats user mapping "0" or "1"
--
-- Returns [immediate leave user IDs..., "---", stale arm tokens...].

local nowSec = tonumber(ARGV[1])
local immediateLeaves = {}

local dueUsers = redis.call("zrangebyscore", KEYS[6], "-inf", nowSec, "LIMIT", 0, tonumber(ARGV[6]))

for i = 1, #dueUsers do
    local userID = dueUsers[i]
    local roster = redis.call("hget", KEYS[7], userID)
    local staleCount = 0
    if roster and roster ~= '' then
        for clientID in string.gmatch(roster, "[^,]+") do
            staleCount = staleCount + 1
            redis.call("hdel", KEYS[5], clientID)
            redis.call("hdel", KEYS[2], clientID)
            redis.call("zrem", KEYS[1], clientID)
            if ARGV[9] ~= '0' then
                local c = redis.call("hincrby", KEYS[4], userID, -1)
                if c <= 0 then
                    redis.call("zrem", KEYS[3], userID)
                    redis.call("hdel", KEYS[4], userID)
                end
            end
        end
    end

    redis.call("zrem", KEYS[6], userID)
    redis.call("hdel", KEYS[7], userID)

    local ttlSec = tonumber(ARGV[8])
    redis.call("expire", KEYS[5], ttlSec)
    redis.call("expire", KEYS[7], ttlSec)

    -- Fire when there was at least one stale client, OR the roster structure
    -- itself expired while the active index lingered (whole-key TTL race after a
    -- hard loss): either way the user has no live connection. Skip if the
    -- presence hash still carries a live client for this user (a refresh landed
    -- between the score check and here).
    local stillLive = false
    if roster and roster ~= '' then
        for clientID in string.gmatch(roster, "[^,]+") do
            if redis.call("hexists", KEYS[2], clientID) == 1 then
                stillLive = true
                break
            end
        end
    end

    if (staleCount > 0 or not roster) and not stillLive and redis.call("hexists", KEYS[8], userID) == 0 then
        local graceMs = tonumber(ARGV[3])
        if graceMs <= 0 then
            immediateLeaves[#immediateLeaves + 1] = userID
        else
            local fireAt = tonumber(ARGV[2]) + graceMs
            local token = tostring(fireAt) .. "|" .. ARGV[5] .. "|" .. userID
            redis.call("hset", KEYS[8], userID, token .. "|" .. tostring(fireAt))
            redis.call("zadd", KEYS[9], fireAt, userID)
            local pendingTTL = tonumber(ARGV[7])
            redis.call("expire", KEYS[8], pendingTTL)
            redis.call("expire", KEYS[9], pendingTTL)
            redis.call("publish", ARGV[4], cjson.encode({
                t = "arm",
                c = ARGV[5],
                u = userID,
                f = fireAt,
                k = token,
            }))
        end
    end
end

-- Recover pending leaves whose fireAt passed but were never confirmed.
local staleArms = {}
local due = redis.call("zrangebyscore", KEYS[9], "-inf", ARGV[2], "LIMIT", 0, tonumber(ARGV[6]))
for i = 1, #due do
    local userID = due[i]
    local record = redis.call("hget", KEYS[8], userID)
    if record then
        local token = string.match(record, "^(.*)|[^|]*$") or record
        local fireAtStr = string.match(record, "|([^|]*)$")
        redis.call("publish", ARGV[4], cjson.encode({
            t = "arm",
            c = ARGV[5],
            u = userID,
            f = tonumber(fireAtStr) or tonumber(ARGV[2]),
            k = token,
        }))
        staleArms[#staleArms + 1] = token
    else
        redis.call("zrem", KEYS[9], userID)
    end
end

local result = {}
for i = 1, #immediateLeaves do
    result[#result + 1] = immediateLeaves[i]
end
result[#result + 1] = "---"
for i = 1, #staleArms do
    result[#result + 1] = staleArms[i]
end
return result
