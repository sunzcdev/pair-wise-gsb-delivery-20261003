# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # Represents time zones that are defined by rules that set out when
  # transitions occur.
  class DataTimezone < InfoTimezone
    # (see Timezone#period_for)
    def period_for(time)
      raise ArgumentError, 'time must be specified' unless time
      timestamp = Timestamp.for(time)
      raise ArgumentError, 'time must have a specified utc_offset' unless timestamp.utc_offset
      info.period_for(timestamp)
    end

    # (see Timezone#periods_for_local)
    def periods_for_local(local_time)
      raise ArgumentError, 'local_time must be specified' unless local_time
      info.periods_for_local(Timestamp.for(local_time, :ignore))
    end

    # (see Timezone#periods_for_local_batch)
    def periods_for_local_batch(local_times)
      raise ArgumentError, 'local_times must be specified' unless local_times

      # Convert each input independently, retaining its original index. Valid
      # inputs are converted to a local Timestamp with an unspecified offset
      # and processed in a single, shared traversal. Invalid inputs leave a
      # LocalPeriodsResult in their original position so that other results are
      # not lost.
      valid_timestamps = []
      valid_indices = []

      results = local_times.each_with_index.map do |local_time, index|
        begin
          raise ArgumentError, 'local_time must be specified' unless local_time
          valid_timestamps << Timestamp.for(local_time, :ignore)
          valid_indices << index
          nil
        rescue ArgumentError => e
          LocalPeriodsResult.new([], e)
        end
      end

      info.periods_for_local_batch(valid_timestamps).each_with_index do |periods, i|
        results[valid_indices[i]] = LocalPeriodsResult.new(periods)
      end

      results
    end

    # (see Timezone#transitions_up_to)
    def transitions_up_to(to, from = nil)
      raise ArgumentError, 'to must be specified' unless to
      to_timestamp = Timestamp.for(to)
      from_timestamp = from && Timestamp.for(from)

      begin
        info.transitions_up_to(to_timestamp, from_timestamp)
      rescue ArgumentError => e
        raise ArgumentError, e.message.gsub('_timestamp', '')
      end
    end

    # Returns the canonical {Timezone} instance for this {DataTimezone}.
    #
    # For a {DataTimezone}, this is always `self`.
    #
    # @return [Timezone] `self`.
    def canonical_zone
      self
    end
  end
end
