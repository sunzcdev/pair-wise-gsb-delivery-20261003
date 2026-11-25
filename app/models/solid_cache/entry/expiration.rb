# frozen_string_literal: true

module SolidCache
  class Entry
    module Expiration
      extend ActiveSupport::Concern

      # The entries an expiry run selected, plus the timestamp the selection was
      # taken at. Rewrites keep the row's id but stamp a newer +created_at+ that
      # is strictly greater than any earlier stamp (see Entry#next_write_timestamp),
      # so comparing against +selected_at+ tells the selected versions apart from
      # any row written since.
      ExpiryCandidates = Struct.new(:ids, :selected_at)

      class_methods do
        def expire(count, max_age:, max_entries:, max_size:)
          if (candidates = expiry_candidates(count, max_age: max_age, max_entries: max_entries, max_size: max_size))
            delete_expiry_candidates(candidates)
          else
            0
          end
        end

        private
          def cache_full?(max_entries:, max_size:)
            if max_entries && max_entries < id_range
              true
            elsif max_size && max_size < estimated_size
              true
            else
              false
            end
          end

          def expiry_candidates(count, max_age:, max_entries:, max_size:)
            cache_full = cache_full?(max_entries: max_entries, max_size: max_size)
            return unless cache_full || max_age

            # In the case of multiple concurrent expiry operations, it is desirable to
            # reduce the overlap of entries being addressed by each. For that reason,
            # retrieve more ids than are being expired, and use random
            # sampling to reduce that number to the actual intended count.
            retrieve_count = count * 3

            # Take the boundary before reading the rows, and let it advance the
            # write clock, so every write that lands after this point is stamped
            # later than the boundary and survives the delete below.
            selected_at = next_write_timestamp

            uncached do
              candidates = order(:id).limit(retrieve_count)

              candidate_ids = if cache_full
                candidates.pluck(:id)
              else
                min_created_at = max_age.seconds.ago

                # We don't have an index on created_at, so we check the timestamps
                # of the small batch we retrieved by id rather than filtering in the
                # database. Entries that were rewritten keep their id but get a new
                # timestamp, so they can turn up in this batch and are dropped here.
                candidates.pluck(:id, :created_at)
                          .filter_map { |id, created_at| id if created_at < min_created_at }
              end

              ExpiryCandidates.new(candidate_ids.sample(count), selected_at) if candidate_ids.any?
            end
          end

          # Delete the selected entries in one statement, so the batch delete is
          # preserved. The +created_at+ bound re-checks that these rows are still
          # the versions that were selected: a write to one of the keys stamps a
          # newer +created_at+, so a concurrently refreshed entry is excluded and
          # keeps the value that was just written. The bound is inclusive, and a
          # row whose stored timestamp is coarser than the Ruby value (legacy rows
          # store whole milliseconds) still compares as older than the boundary.
          def delete_expiry_candidates(candidates)
            where(id: candidates.ids).where(created_at: ..candidates.selected_at).delete_all
          end
      end
    end
  end
end
