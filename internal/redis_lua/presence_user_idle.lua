-- Report whether a channel no longer holds any user-presence state so the
-- active-channel registry entry can be removed by the caller (the registry
-- key lives on a different Redis Cluster hash slot, so SREM can't run here).
--
-- KEYS[1] - presence hash key
-- KEYS[2] - per-user connection count hash key
-- KEYS[3] - pending user leave zset key
--
-- Returns: 1 when the channel is idle, 0 otherwise.

if redis.call("hlen", KEYS[1]) == 0 and
   redis.call("hlen", KEYS[2]) == 0 and
   redis.call("zcard", KEYS[3]) == 0 then
    return 1
end
return 0
