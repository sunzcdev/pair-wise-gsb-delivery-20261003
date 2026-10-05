-- publish_idempotent publishes one publication whose result is remembered, so
-- that a retry carrying the same idempotency key does not publish it twice.
--
-- The logic is a function with one definition, and the caller supplies the key
-- and arguments explicitly - nothing here reads KEYS or ARGV.
--
-- Redis executes this whole script atomically. The content hash and result are
-- committed with the publication, and a strict retry verifies that hash first.
local function publish_idempotent(result_key, payload, channel, publish_command, result_key_expire, strict, content_hash)
    if result_key_expire ~= '' then
        local cached_result = redis.call("hmget", result_key, "e", "s", "h")
        local result_epoch, result_offset, result_hash = cached_result[1], cached_result[2], cached_result[3]
        if result_epoch ~= false then
            if strict == "1" and result_hash ~= content_hash then
                return redis.error_reply("__centrifuge_idempotency_conflict__")
            end
            return { result_offset, result_epoch }
        end
    end

    local res
    if channel ~= '' then
        res = redis.call(publish_command, channel, payload)
    end

    if result_key_expire ~= '' then
        redis.call("hset", result_key, "e", "", "h", content_hash)
        redis.call("expire", result_key, result_key_expire)
    end

    return res
end
