-- add_history appends one publication to a channel's history stream and
-- publishes it, returning { offset, epoch, from_cache, skipped }.
--
-- The logic is a function with one definition, and the caller supplies the
-- keys and arguments explicitly - nothing here reads KEYS or ARGV.
--
-- Redis executes this whole script atomically. The cached content hash and
-- result are committed together only after the history entry and publication
-- have both been prepared, so a caller never sees one without the other.
local function add_history(
    stream_key, meta_key, result_key,
    message_payload, stream_size, stream_ttl, channel, meta_expire,
    new_epoch_if_empty, publish_command, result_key_expire, use_delta,
    version, version_epoch, strict, content_hash
)
    if result_key_expire ~= '' then
        local cached_result = redis.call("hmget", result_key, "e", "s", "h")
        local result_epoch, result_offset, result_hash = cached_result[1], cached_result[2], cached_result[3]
        if result_epoch ~= false then
            if strict == "1" and result_hash ~= content_hash then
                return redis.error_reply("__centrifuge_idempotency_conflict__")
            end
            return { result_offset, result_epoch, "1", "0" }
        end
    end

    local current_epoch = redis.call("hget", meta_key, "e")
    if current_epoch == false then
        current_epoch = new_epoch_if_empty
        redis.call("hset", meta_key, "e", current_epoch)
    end

    if version ~= "0" then
        local prev_version_values = redis.call("hmget", meta_key, "v", "ve", "s")
        local prev_version = prev_version_values[1]
        local prev_version_epoch = prev_version_values[2]
        local current_offset = prev_version_values[3]
        if prev_version then
            if (version_epoch == "" or version_epoch == prev_version_epoch) and (tonumber(prev_version) >= tonumber(version)) then
                -- Suppressed: return current offset without incrementing
                local offset_num = 0
                if current_offset then
                    offset_num = tonumber(current_offset)
                end
                return { offset_num, current_epoch, "0", "1" }
            end
        end
        redis.call("hset", meta_key, "v", version, "ve", version_epoch)
    end

    local top_offset = redis.call("hincrby", meta_key, "s", 1)

    if meta_expire ~= '0' then
        redis.call("expire", meta_key, meta_expire)
    end

    local prev_message_payload = ""
    if use_delta == "1" and top_offset ~= 1 then
        local prev_entries = redis.call("xrevrange", stream_key, "+", "-", "COUNT", 1)
        if #prev_entries > 0 then
            prev_message_payload = prev_entries[1][2]["d"]
            local fields_and_values = prev_entries[1][2]
            -- Loop through the fields and values to find the field "d"
            for i = 1, #fields_and_values, 2 do
                local field = fields_and_values[i]
                local value = fields_and_values[i + 1]
                if field == "d" then
                    prev_message_payload = value
                    break -- Stop the loop once we find the field "d"
                end
            end
        end
    end

    if top_offset == 1 then
        -- If a new epoch starts (thus top_offset is 1), try to delete the existing stream, this may
        -- be important when the meta key is evicted by Redis LRU/LFU strategies. So we are emulating
        -- an eviction of stream key here.
        redis.call("del", stream_key)
        prev_message_payload = ""
    end

    redis.call("xadd", stream_key, "MAXLEN", stream_size, top_offset, "d", message_payload)
    redis.call("expire", stream_key, stream_ttl)

    if channel ~= '' then
        local payload
        if use_delta == "1" then
            payload = "__" ..
            "d1:" ..
            top_offset ..
            ":" ..
            current_epoch ..
            ":" .. #prev_message_payload .. ":" .. prev_message_payload .. ":" .. #message_payload .. ":" .. message_payload
        else
            payload = "__" .. "p1:" .. top_offset .. ":" .. current_epoch .. "__" .. message_payload
        end
        redis.call(publish_command, channel, payload)
    end

    -- The content hash and remembered result are committed in the same HSET.
    if result_key_expire ~= '' then
        redis.call("hset", result_key, "e", current_epoch, "s", top_offset, "h", content_hash)
        redis.call("expire", result_key, result_key_expire)
    end

    return { top_offset, current_epoch, "0", "0" }
end
