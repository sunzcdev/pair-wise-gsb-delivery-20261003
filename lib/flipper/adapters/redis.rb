require 'set'
require 'redis'
require 'flipper'
require 'flipper/adapters/redis_shared/methods'

module Flipper
  module Adapters
    class Redis
      include ::Flipper::Adapter
      include ::Flipper::Adapters::RedisShared

      attr_reader :key_prefix

      def features_key
        "#{key_prefix}flipper_features"
      end

      def key_for(feature_name)
        "#{key_prefix}#{feature_name}"
      end

      # Public: Initializes a Redis flipper adapter.
      #
      # client - The Redis client to use.
      # key_prefix - an optional prefix with which to namespace
      #              flipper's Redis keys
      def initialize(client, key_prefix: nil)
        @client = client
        @key_prefix = key_prefix
        @sadd_returns_boolean = with_connection do |conn|
          conn.class.respond_to?(:sadd_returns_boolean) && conn.class.sadd_returns_boolean
        end
      end

      # Public: The set of known features.
      def features
        read_feature_keys
      end

      # Public: Adds a feature to the set of known features.
      def add(feature)
        ensure_not_reserved_key!(feature)
        if redis_sadd_returns_boolean?
          with_connection { |conn| conn.sadd? features_key, feature.key }
        else
          with_connection { |conn| conn.sadd features_key, feature.key }
        end
        true
      end

      # Public: Removes a feature from the set of known features.
      def remove(feature)
        if redis_sadd_returns_boolean?
          with_connection { |conn| conn.srem? features_key, feature.key }
        else
          with_connection { |conn| conn.srem features_key, feature.key }
        end
        # Removing a reserved name left in the list must not delete the list.
        with_connection { |conn| conn.del key_for(feature.key) } unless reserved_key?(feature)
        true
      end

      # Public: Clears the gate values for a feature.
      def clear(feature)
        ensure_not_reserved_key!(feature)
        with_connection { |conn| conn.del key_for(feature.key) }
        true
      end

      # Public: Gets the values for all gates for a given feature.
      #
      # Returns a Hash of Flipper::Gate#key => value.
      def get(feature)
        doc = doc_for(feature)
        result_for_feature(feature, doc)
      end

      def get_multi(features)
        read_many_features(features)
      end

      def get_all(**kwargs)
        features = read_feature_keys.map { |key| Flipper::Feature.new(key, self) }
        read_many_features(features)
      end

      # Public: The Flipper::SyncVersion of the flags last synced into this
      # adapter, shared by every process using the same Redis.
      def sync_version
        SyncVersion.parse(with_connection { |conn| conn.get(sync_version_key) })
      end

      # Public: Records the Flipper::SyncVersion of the flags just synced.
      def sync_version=(version)
        with_connection { |conn| conn.set(sync_version_key, version.to_s) }
      end

      # Public: Enables a gate for a given thing.
      #
      # feature - The Flipper::Feature for the gate.
      # gate - The Flipper::Gate to enable.
      # thing - The Flipper::Type being enabled for the gate.
      #
      # Returns true.
      def enable(feature, gate, thing)
        ensure_not_reserved_key!(feature)
        feature_key = key_for(feature.key)
        case gate.data_type
        when :boolean
          clear(feature)
          with_connection { |conn| conn.hset feature_key, gate.key, thing.value.to_s }
        when :integer
          with_connection { |conn| conn.hset feature_key, gate.key, thing.value.to_s }
        when :set
          with_connection { |conn| conn.hset feature_key, to_field(gate, thing), 1 }
        when :json
          with_connection { |conn| conn.hset feature_key, gate.key, Typecast.to_json(thing.value) }
        else
          unsupported_data_type gate.data_type
        end

        true
      end

      # Public: Disables a gate for a given thing.
      #
      # feature - The Flipper::Feature for the gate.
      # gate - The Flipper::Gate to disable.
      # thing - The Flipper::Type being disabled for the gate.
      #
      # Returns true.
      def disable(feature, gate, thing)
        ensure_not_reserved_key!(feature)
        feature_key = key_for(feature.key)
        case gate.data_type
        when :boolean
          with_connection { |conn| conn.del feature_key }
        when :integer
          with_connection { |conn| conn.hset feature_key, gate.key, thing.value.to_s }
        when :set
          with_connection { |conn| conn.hdel feature_key, to_field(gate, thing) }
        when :json
          with_connection { |conn| conn.hdel feature_key, gate.key }
        else
          unsupported_data_type gate.data_type
        end

        true
      end

      # Public: Replaces one member of a set gate with another as one atomic
      # server-side change.
      #
      # A small Lua script checks the members, then removes the old member and
      # adds the new one in the same step. No other client can observe the set
      # without the old member and before the new one exists, two clients
      # cannot interleave into a set that is missing a member neither asked to
      # remove, and the set never grows when the change is a swap.
      #
      # The script validates the caller's expected members against the set it
      # is about to change, so a client working from a stale member list
      # changes nothing and gets Flipper::ActorSetConflict, while the client
      # whose members still match applies the change. That is what makes two
      # concurrent replacements deterministic across processes.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        ensure_not_reserved_key!(feature)
        unsupported_data_type(gate.data_type) unless gate.data_type == :set

        prefix = "#{gate.key}/"
        expected_members = normalize_expected_members(expected)
        argv = [prefix, old_thing.value.to_s, new_thing.value.to_s]
        argv << (expected_members ? expected_members.size : -1)
        argv.concat(expected_members.to_a.sort) if expected_members

        code = with_connection do |conn|
          run_replace_script(conn, REPLACE_SCRIPT, key_for(feature.key), argv)
        end.to_i

        case code
        when 1, 3
          true
        when 0
          raise ActorSetConflict.new(
            feature_key: feature.key,
            expected: expected_members,
            actual: normalize_member_values(get(feature)[gate.key]),
          )
        when 2
          raise ActorSetConflict.new(
            "Cannot replace member #{old_thing.value.inspect} with #{new_thing.value.inspect} " \
              "on feature #{feature.key.inspect} because neither is enabled. " \
              "Re-read the members and retry if another replacement already ran.",
            feature_key: feature.key,
            expected: expected_members,
            actual: normalize_member_values(get(feature)[gate.key]),
          )
        else
          raise Flipper::Error, "unexpected response #{code.inspect} from actor replacement script"
        end
      end

      private

      # Private: Replaces one member of a :set gate with another in one
      # server-side step.
      #
      # KEYS[1] - the feature hash key.
      # ARGV[1] - field prefix ("<gate key>/").
      # ARGV[2] - value of the member that should leave.
      # ARGV[3] - value of the member that should join.
      # ARGV[4] - expected member count, or -1 when no expectation was given.
      # ARGV[5..] - the expected member values, sorted.
      #
      # Returns 1 when the replacement was applied, 3 when it was already the
      # case, 0 when the expected members do not match the set, and 2 when
      # neither the old nor the new member is present.
      REPLACE_SCRIPT = <<~LUA.freeze
        local key = KEYS[1]
        local prefix = ARGV[1]
        local old_field = prefix .. ARGV[2]
        local new_field = prefix .. ARGV[3]
        local expected_count = tonumber(ARGV[4])

        if expected_count >= 0 then
          local current_count = 0
          for _, field in ipairs(redis.call("HKEYS", key)) do
            if string.sub(field, 1, string.len(prefix)) == prefix then
              current_count = current_count + 1
            end
          end
          if current_count ~= expected_count then
            return 0
          end
          for i = 5, 4 + expected_count do
            if redis.call("HEXISTS", key, prefix .. ARGV[i]) == 0 then
              return 0
            end
          end
        end

        local has_old = redis.call("HEXISTS", key, old_field)
        local has_new = redis.call("HEXISTS", key, new_field)
        if has_old == 0 and has_new == 0 then
          return 2
        end

        redis.call("HDEL", key, old_field)
        redis.call("HSET", key, new_field, 1)
        return 1
      LUA

      # Private: Runs the replacement script, tolerating the positional and
      # keyword call styles of the supported redis-rb versions.
      def run_replace_script(conn, script, key, argv)
        unless conn.respond_to?(:eval)
          raise Flipper::Error, "the configured Redis client does not support EVAL, which is required to replace actors atomically"
        end

        begin
          conn.eval(script, keys: [key], argv: argv)
        rescue ArgumentError
          # redis-rb < 4.6 took keys and argv positionally.
          conn.eval(script, [key], argv)
        end
      end

      # Private: The feature list and sync version live at keys next to the
      # feature data, so a feature with the same key would overwrite or
      # delete them.
      def reserved_key?(feature)
        Flipper.reserved_key?(feature.key) || key_for(feature.key) == features_key
      end

      def ensure_not_reserved_key!(feature)
        super
        return unless key_for(feature.key) == features_key

        raise ArgumentError, "#{feature.key.inspect} is reserved by the Redis adapter and cannot be used as a feature name"
      end

      def sync_version_key
        key_for(SyncVersion::KEY)
      end

      def redis_sadd_returns_boolean?
        @sadd_returns_boolean
      end

      def read_many_features(features)
        docs = docs_for(features)
        result = {}
        features.zip(docs) do |feature, doc|
          result[feature.key] = result_for_feature(feature, doc)
        end
        result
      end

      def read_feature_keys
        with_connection { |conn| conn.smembers(features_key).to_set }
      end

      # Private: Gets a hash of fields => values for the given feature.
      #
      # Returns a Hash of fields => values.
      def doc_for(feature, pipeline: nil)
        if pipeline
          pipeline.hgetall(key_for(feature.key))
        else
          with_connection { |conn| conn.hgetall(key_for(feature.key)) }
        end
      end

      def docs_for(features)
        with_connection do |conn|
          conn.pipelined do |pipeline|
            features.each do |feature|
              doc_for(feature, pipeline: pipeline)
            end
          end
        end
      end

      def result_for_feature(feature, doc)
        result = {}
        fields = doc.keys

        feature.gates.each do |gate|
          result[gate.key] =
            case gate.data_type
            when :boolean, :integer
              doc[gate.key.to_s]
            when :set
              fields_to_gate_value fields, gate
            when :json
              value = doc[gate.key.to_s]
              Typecast.from_json(value)
            else
              unsupported_data_type gate.data_type
            end
        end

        result
      end

      # Private: Converts gate and thing to hash key.
      def to_field(gate, thing)
        "#{gate.key}/#{thing.value}"
      end

      # Private: Returns a set of values given an array of fields and a gate.
      #
      # Returns a Set of the values enabled for the gate.
      def fields_to_gate_value(fields, gate)
        regex = %r{^#{Regexp.escape(gate.key.to_s)}/}
        keys = fields.grep(regex)
        values = keys.map { |key| key.split('/', 2).last }
        values.to_set
      end

      # Private
      def unsupported_data_type(data_type)
        raise "#{data_type} is not supported by this adapter"
      end
    end
  end
end

Flipper.configure do |config|
  config.adapter do
    client = Redis.new(url: ENV["FLIPPER_REDIS_URL"] || ENV["REDIS_URL"] || "redis://localhost:6379")
    Flipper::Adapters::Redis.new(client)
  end
end
