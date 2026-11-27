# frozen_string_literal: true

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
      end

      private
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
            Entry.lock_and_write(key) do |value|
              block.call(value).tap { |result| track_writes(1) if result }
            end
          end
        end

        # Runs one locked batch per shard. Yields the group's changes, their payloads and
        # the batch receipt payload; returns [resolutions, writes] for the group. Returns
        # a [connection, changes, resolutions, error] tuple per shard so the caller can
        # attribute an unavailable shard to its keys.
        def entry_adjust_multi(changes, token:, options:)
          receipt_key = receipt_key_for(token, options)

          entry_locked_batches(changes, receipt_key: receipt_key, failsafe: :adjust_multi) do |grouped_changes, payloads, receipt_payload|
            resolve_adjustments(grouped_changes, payloads, receipt_payload, receipt_key, token, options)
          end
        end

        def entry_locked_batches(changes, receipt_key:, failsafe:)
          changes_by_key = changes.index_by(&:key)

          group_by_connection(changes.map(&:key)).map do |connection, keys|
            grouped_changes = keys.map { |key| changes_by_key.fetch(key) }
            next [ connection, grouped_changes, {}, nil ] if grouped_changes.empty?

            resolutions, error = failsafe_result(failsafe) do
              group_resolutions = nil

              with_connection(connection) do
                Entry.locked_batch(keys + [ receipt_key ]) do |payloads|
                  group_resolutions, writes = yield(grouped_changes, payloads, payloads[receipt_key])
                  track_writes(writes.count { |key, payload| payload && key != receipt_key })
                  writes
                end
              end

              group_resolutions
            end

            [ connection, grouped_changes, resolutions, error ]
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
            Entry.write(key, payload)
            track_writes(1)
            true
          end
        end

        def entry_write_multi(entries)
          writing_keys(entries, failsafe: :write_multi_entries, failsafe_returning: false) do |entries|
            Entry.write_multi(entries)
            track_writes(entries.count)
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
    end
  end
end
