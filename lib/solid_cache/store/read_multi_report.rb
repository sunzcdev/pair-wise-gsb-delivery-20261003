# frozen_string_literal: true

module SolidCache
  class Store
    # The result of a Store#read_multi_report call.
    #
    # A plain multi read collapses every non-hit into "absent from the returned
    # hash", which is ambiguous when a shard was unreachable and when a key was
    # stored with a nil value. This report keeps those cases apart:
    #
    #   * #hits - the key was found (the value may legitimately be nil)
    #   * #misses - the shard answered and the key is not there
    #   * #unknown - the shard could not be read, so nothing is known
    #
    # A key only ends up in #unknown when the shard holding it failed, so hits
    # and misses for the healthy shards can be used directly.
    class ReadMultiReport
      include Enumerable

      # Per shard detail. +status+ is :ok when the shard answered (even if some
      # of its keys were missing) and :unavailable when the read failed.
      Shard = Struct.new(:name, :names, :status, :error, keyword_init: true) do
        def ok?
          status == :ok
        end

        def unavailable?
          status == :unavailable
        end
      end

      attr_reader :names

      def initialize(names: [])
        @names = names
        @values = {}
        @statuses = {}
        @shards = {}
        @shards_for_names = {}
      end

      # The values of the keys that hit, including keys stored with a nil value.
      # This is the hash a caller can consume directly.
      def results
        @values.dup
      end

      # Iterates over #results, so the report can be used loosely like the hash
      # returned by Store#read_multi.
      def each(&block)
        results.each(&block)
      end

      def hits
        names.select { |name| hit?(name) }
      end

      def misses
        names.select { |name| miss?(name) }
      end

      # Keys whose shard could not be read. Never includes keys from a shard
      # that answered, even when that answer was "not present".
      def unknown
        names.select { |name| unknown?(name) }
      end

      # :hit, :miss or :unknown
      def status(name)
        @statuses.fetch(name, :unknown)
      end

      def hit?(name)
        status(name) == :hit
      end

      def miss?(name)
        status(name) == :miss
      end

      def unknown?(name)
        status(name) == :unknown
      end

      # The value for a key that hit, which may be nil.
      def value(name)
        @values[name]
      end

      # The shard the key was assigned to.
      def shard_for(name)
        @shards_for_names[name]
      end

      # Per shard detail: { shard_name => ReadMultiReport::Shard }.
      def shards
        @shards.dup
      end

      def shard(name)
        @shards[name]
      end

      # The shards that could not be read at all.
      def unavailable_shards
        @shards.values.select(&:unavailable?)
      end

      # The error raised while reading a shard, if any.
      def error_for(name)
        shard(name)&.error
      end

      # False when any shard failed, i.e. part of the read is unknown.
      def complete?
        unknown.empty?
      end

      def to_h
        results
      end

      # @api private
      def record_hit(shard, name, value)
        record(shard, name, :hit)
        @values[name] = value
      end

      # @api private
      def record_miss(shard, name)
        record(shard, name, :miss)
        @values.delete(name)
      end

      # @api private
      def record_available_shard(shard, shard_names)
        @shards[shard] = Shard.new(name: shard, names: shard_names, status: :ok, error: nil)
      end

      # @api private
      def record_unavailable_shard(shard, shard_names, error:)
        @shards[shard] = Shard.new(name: shard, names: shard_names, status: :unavailable, error: error)

        shard_names.each do |name|
          @statuses[name] = :unknown
          @values.delete(name)
          @shards_for_names[name] = shard
        end
      end

      private
        def record(shard, name, status)
          @statuses[name] = status
          @shards_for_names[name] = shard
        end
    end
  end
end
