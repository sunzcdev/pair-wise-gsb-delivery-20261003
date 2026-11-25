# frozen_string_literal: true

require "monitor"

module SolidCache
  class Store
    module Entries
      attr_reader :clear_with

      def initialize(options = {})
        super(options)

        # Truncating in test mode breaks transactional tests in MySQL (not in Postgres though)
        @clear_with = options.fetch(:clear_with) { Rails.env.test? ? :delete : :truncate }&.to_sym

        unless [ :truncate, :delete ].include?(clear_with)
          raise ArgumentError, "`clear_with` must be either ``:truncate`` or ``:delete`"
        end

        # Per-shard, reentrant locks serializing a generation bump against the
        # writes that stamp that generation, so a write can never keep an old
        # generation after the bump has committed on its shard. Reentrant so a
        # sweep gate that itself performs writes does not deadlock.
        @generation_locks = Hash.new { |locks, shard| locks[shard] = Monitor.new }
      end

      private
        def generation_lock(shard)
          @generation_locks[shard]
        end

        def entry_clear
          writing_all(failsafe: :clear, failsafe_returning: nil) do
            if clear_with == :truncate
              Entry.clear_truncate
            else
              Entry.clear_delete
            end
          end
        end

        def entry_lock_and_write(key, &block)
          writing_key(key, failsafe: :increment) do
            generation_lock(Entry.current_shard).synchronize do
              Entry.lock_and_write(key, namespace: write_generation_namespace) do |value|
                block.call(value).tap { |result| track_writes(1) if result }
              end
            end
          end
        end

        def entry_read(key)
          reading_key(key, failsafe: :read_entry) do
            Entry.read(key)
          end
        end

        def entry_read_multi(keys)
          reading_keys(keys, failsafe: :read_multi_mget, failsafe_returning: {}) do |keys|
            Entry.read_multi(keys)
          end
        end

        def entry_write(key, payload)
          writing_key(key, failsafe: :write_entry, failsafe_returning: nil) do
            generation_lock(Entry.current_shard).synchronize do
              Entry.write(key, payload, generation: current_generation)
              track_writes(1)
            end
            true
          end
        end

        def entry_write_multi(entries)
          writing_keys(entries, failsafe: :write_multi_entries, failsafe_returning: false) do |entries|
            generation_lock(Entry.current_shard).synchronize do
              Entry.write_multi(stamp_generation(entries))
              track_writes(entries.count)
            end
            true
          end
        end

        def entry_delete(key)
          writing_key(key, failsafe: :delete_entry, failsafe_returning: false) do
            Entry.delete_by_key(key) > 0
          end
        end

        def entry_delete_multi(entries)
          writing_keys(entries, failsafe: :delete_multi_entries, failsafe_returning: 0) do
            Entry.delete_by_key(*entries)
          end
        end

        # Generation currently assigned to writes on the active shard. This runs
        # inside the shard connection established by writing_key/writing_keys.
        def current_generation
          Generation.current(write_generation_namespace)
        end

        def stamp_generation(entries)
          generation = current_generation
          entries.map { |entry| entry.merge(generation: generation) }
        end

        def write_generation_namespace
          options[:namespace] || Generation::DEFAULT_NAMESPACE
        end
    end
  end
end
