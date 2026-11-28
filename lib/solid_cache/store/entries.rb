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
            # Checked as late as possible, just before each shard's SQL
            # write: entries that have already expired are dropped so the
            # late write can't overwrite a live value stored in the meantime.
            unexpired_entries = entries.reject { |entry| expired_payload?(entry) }
            if unexpired_entries.any?
              Entry.write_multi(unexpired_entries.map { |entry| entry.except(:expires_at) })
              track_writes(unexpired_entries.count)
            end
            unexpired_entries.count == entries.count
          end
        end

        def expired_payload?(payload)
          payload[:expires_at] && payload[:expires_at] <= Time.now.to_f
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
