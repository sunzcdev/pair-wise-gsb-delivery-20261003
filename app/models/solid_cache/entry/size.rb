# frozen_string_literal: true

module SolidCache
  class Entry
    module Size
      extend ActiveSupport::Concern

      # The counter that tracks the estimated size of the cache on each shard
      SIZE_COUNTER_NAME = "size"

      included do
        scope :largest_byte_sizes, -> (limit) { from(order(byte_size: :desc).limit(limit).select(:byte_size)) }
        scope :in_key_hash_range, -> (range) { where(key_hash: range) }
        scope :up_to_byte_size, -> (cutoff) { where("byte_size <= ?", cutoff) }
      end

      class_methods do
        # The estimated size of the cache in bytes.
        #
        # By default this reads a per-shard counter that is kept up to date by
        # applying the byte size delta of every successful write, overwrite and
        # delete, so it is available without touching the entries table.
        #
        # Pass <tt>samples:</tt> to get a bounded sampling estimate instead.
        def estimated_size(samples: nil)
          if samples
            sampled_estimated_size(samples: samples)
          else
            Counter.read(SIZE_COUNTER_NAME)
          end
        end

        # A bounded sampling estimate of the cache size, averaging out outliers
        def sampled_estimated_size(samples: SolidCache.configuration.size_estimate_samples)
          MovingAverageEstimate.new(samples: samples).size
        end

        # Corrects the tracked estimate with a bounded sampling estimate, to
        # repair any drift caused by concurrent writes or untracked deletes
        def reconcile_estimated_size(samples: SolidCache.configuration.size_estimate_samples)
          Counter.set(SIZE_COUNTER_NAME, amount: sampled_estimated_size(samples: samples))
        end

        def clear_estimated_size
          Counter.set(SIZE_COUNTER_NAME, amount: 0)
        end

        def track_byte_size(amount)
          Counter.increment(SIZE_COUNTER_NAME, amount: amount) unless amount.zero?
        end
      end
    end
  end
end
