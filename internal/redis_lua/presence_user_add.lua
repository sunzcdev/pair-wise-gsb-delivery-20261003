-- Add/update presence information with optional per-user presence events.
-- Extends presence_add.lua with atomic per-user connection counting and the
-- user join/cancel decisions. Every transition is decided here, so two nodes
-- updating at the same time can never double-count or double-join.
--
-- KEYS[1] - presence zset key (client expire index)
-- KEYS[2] - presence hash key (clientID -> info)
-- KEYS[3] - per-user stats zset key (existing PresenceStats optimization)
-- KEYS[4] - per-user stats hash key
-- KEYS[5] - user events client map hash key (clientID -> userID)
-- KEYS[6] - user events active users zset key (userID -> latest expire second)
-- KEYS[7] - user events roster hash key (userID -> CSV of client IDs)
-- KEYS[8] - user events pending leave hash key (userID -> token|fireAtMs)
-- KEYS[9] - user events pending fire-at zset key (userID -> fireAtMs)
--
-- ARGV[1]  - key expire seconds
-- ARGV[2]  - expire at (unix seconds) for this client
-- ARGV[3]  - client ID
-- ARGV[4]  - info payload
-- ARGV[5]  - user ID
-- ARGV[6]  - enable stats user mapping "0" or "1"
-- ARGV[7]  - use hash field TTL "0" or "1"
-- ARGV[8]  - enable user presence events "0" or "1"
-- ARGV[9]  - PUB/SUB channel for leave scheduling/cancellation
-- ARGV[10] - presence TTL seconds (to keep event keys alive)
-- ARGV[11] - channel name (for the PUB/SUB payload)

-- Check if client ID is new (for the pre-existing stats mapping).
local isNewClient = false
if ARGV[6] ~= '0' then
    isNewClient = redis.call("hexists", KEYS[2], ARGV[3]) == 0
end

-- Add per-client presence.
redis.call("hset", KEYS[2], ARGV[3], ARGV[4])
redis.call("expire", KEYS[2], ARGV[1])
if ARGV[7] == '0' then
    redis.call("zadd", KEYS[1], tonumber(ARGV[2]), ARGV[3])
    redis.call("expire", KEYS[1], ARGV[1])
else
    redis.call("hexpire", KEYS[2], ARGV[1], "FIELDS", "1", ARGV[3])
end

-- Maintain the pre-existing per-user stats mapping (unchanged behavior).
if ARGV[6] ~= '0' then
    if isNewClient then
        redis.call("hincrby", KEYS[4], ARGV[5], 1)
    end
    redis.call("expire", KEYS[4], ARGV[1])
    if ARGV[7] == '0' then
        redis.call("zadd", KEYS[3], tonumber(ARGV[2]), ARGV[5])
        redis.call("expire", KEYS[3], ARGV[1])
    else
        redis.call("hexpire", KEYS[4], ARGV[1], "FIELDS", "1", ARGV[5])
    end
end

-- Per-user presence events.
-- Returns 1 when this add brought the user in (count 0 -> 1).
if ARGV[8] ~= '0' and ARGV[5] ~= '' then
    local ttlSec = tonumber(ARGV[10])
    local isNewUserClient = redis.call("hexists", KEYS[5], ARGV[3]) == 0

    if isNewUserClient then
        -- KEYS[5] maps clientID -> userID for O(1) membership; KEYS[7]'s
        -- companion rosters hash (field userID, CSV client IDs) lets the sweep
        -- enumerate one expired user's clients without a full scan. The client
        -- count is the roster size.
        redis.call("hset", KEYS[5], ARGV[3], ARGV[5])
        redis.call("expire", KEYS[5], ttlSec)

        local roster = redis.call("hget", KEYS[7], ARGV[5])
        local members = {}
        local count = 0
        if roster and roster ~= '' then
            for id in string.gmatch(roster, "[^,]+") do
                members[#members + 1] = id
                count = count + 1
            end
        end
        members[#members + 1] = ARGV[3]
        count = count + 1
        redis.call("hset", KEYS[7], ARGV[5], table.concat(members, ","))
        redis.call("expire", KEYS[7], ttlSec)
        -- Active index always uses a zset, independent of UseHashFieldTTL: the
        -- user field must survive as long as ANY of that user's connections is
        -- refreshing, not expire on one client's field TTL.
        redis.call("zadd", KEYS[6], tonumber(ARGV[2]), ARGV[5])
        redis.call("expire", KEYS[6], ttlSec)

        local pending = redis.call("hget", KEYS[8], ARGV[5])
        if pending then
            -- Grace period running (possibly armed on another node). Reconnect
            -- cancels it cluster-wide.
            redis.call("hdel", KEYS[8], ARGV[5])
            redis.call("zrem", KEYS[9], ARGV[5])
            local token = string.match(pending, "^(.*)|[^|]*$") or pending
            redis.call("publish", ARGV[9], cjson.encode({
                t = "cancel",
                c = ARGV[11],
                u = ARGV[5],
                k = token,
            }))
        elseif count == 1 then
            return 1
        end
    end
end

return 0
