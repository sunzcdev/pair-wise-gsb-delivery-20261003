-- Remove client presence with optional per-user presence events.
-- Extends presence_rem.lua with roster-based per-user counting and leave grace
-- arming. Two nodes removing their last connections concurrently cannot both
-- arm or both fire: the whole decision is this one atomic script.
--
-- KEYS[1] - presence set key
-- KEYS[2] - presence hash key
-- KEYS[3] - per-user stats zset key
-- KEYS[4] - per-user stats hash key
-- KEYS[5] - user events client map hash key (clientID -> userID)
-- KEYS[6] - user events active users zset key
-- KEYS[7] - user events roster hash key (userID -> CSV of client IDs)
-- KEYS[8] - user events pending leave hash key
-- KEYS[9] - user events pending fire-at zset key
--
-- ARGV[1]  - client ID
-- ARGV[2]  - user ID
-- ARGV[3]  - enable stats user mapping "0" or "1"
-- ARGV[4]  - use hash field TTL "0" or "1"
-- ARGV[5]  - enable user presence events "0" or "1"
-- ARGV[6]  - PUB/SUB channel for leave scheduling
-- ARGV[7]  - leave grace period milliseconds ("0" = immediate)
-- ARGV[8]  - current unix milliseconds
-- ARGV[9]  - pending leave record TTL seconds
-- ARGV[10] - presence TTL seconds (key keep-alive)
-- ARGV[11] - channel name (payload)

local clientExists = false
if ARGV[3] ~= '0' then
    clientExists = redis.call("hexists", KEYS[2], ARGV[1]) == 1
end

redis.call("hdel", KEYS[2], ARGV[1])
if ARGV[4] == '0' then
    redis.call("zrem", KEYS[1], ARGV[1])
end

if ARGV[3] ~= '0' and clientExists then
    local connectionsCount = redis.call("hincrby", KEYS[4], ARGV[2], -1)
    if connectionsCount <= 0 then
        if ARGV[4] == '0' then
            redis.call("zrem", KEYS[3], ARGV[2])
        end
        redis.call("hdel", KEYS[4], ARGV[2])
    end
end

-- Per-user presence events. Returns 1 when the leave fires immediately.
if ARGV[5] ~= '0' and ARGV[2] ~= '' then
    if redis.call("hexists", KEYS[5], ARGV[1]) == 1 then
        redis.call("hdel", KEYS[5], ARGV[1])

        local roster = redis.call("hget", KEYS[7], ARGV[2])
        local remaining = {}
        local removed = false
        if roster and roster ~= '' then
            for id in string.gmatch(roster, "[^,]+") do
                if id == ARGV[1] then
                    removed = true
                else
                    remaining[#remaining + 1] = id
                end
            end
        end

        local ttlSec = tonumber(ARGV[10])
        if #remaining == 0 then
            redis.call("hdel", KEYS[7], ARGV[2])
        else
            redis.call("hset", KEYS[7], ARGV[2], table.concat(remaining, ","))
        end
        redis.call("expire", KEYS[7], ttlSec)
        redis.call("expire", KEYS[5], ttlSec)

        if removed and #remaining == 0 then
            redis.call("zrem", KEYS[6], ARGV[2])

            local graceMs = tonumber(ARGV[7])
            if graceMs <= 0 then
                return 1
            end

            if redis.call("hexists", KEYS[8], ARGV[2]) == 0 then
                local fireAt = tonumber(ARGV[8]) + graceMs
                local token = tostring(fireAt) .. "|" .. ARGV[11] .. "|" .. ARGV[2]
                redis.call("hset", KEYS[8], ARGV[2], token .. "|" .. tostring(fireAt))
                redis.call("zadd", KEYS[9], fireAt, ARGV[2])
                local pendingTTL = tonumber(ARGV[9])
                redis.call("expire", KEYS[8], pendingTTL)
                redis.call("expire", KEYS[9], pendingTTL)

                redis.call("publish", ARGV[6], cjson.encode({
                    t = "arm",
                    c = ARGV[11],
                    u = ARGV[2],
                    f = fireAt,
                    k = token,
                }))
            end
        end
    end
end

return 0
