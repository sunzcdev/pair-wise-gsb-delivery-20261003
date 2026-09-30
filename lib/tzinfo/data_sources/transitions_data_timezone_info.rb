# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  module DataSources
    # Represents a data time zone defined by a list of transitions that change
    # the locally observed time.
    class TransitionsDataTimezoneInfo < DataTimezoneInfo
      # @return [Array<TimezoneTransition>] the transitions that define this
      #   time zone in order of ascending timestamp.
      attr_reader :transitions

      # Initializes a new {TransitionsDataTimezoneInfo}.
      #
      # The passed in `identifier` instance will be frozen. A reference to the
      # passed in `Array` will be retained.
      #
      # The `transitions` `Array` must be sorted in order of ascending
      # timestamp. Each transition must have a
      # {TimezoneTransition#timestamp_value timestamp_value} that is greater
      # than the {TimezoneTransition#timestamp_value timestamp_value} of the
      # prior transition.
      #
      # @param identifier [String] the identifier of the time zone.
      # @param transitions [Array<TimezoneTransitions>] an `Array` of
      #   transitions that each indicate when a change occurs in the locally
      #   observed time.
      # @raise [ArgumentError] if `identifier` is `nil`.
      # @raise [ArgumentError] if `transitions` is `nil`.
      # @raise [ArgumentError] if `transitions` is an empty `Array`.
      def initialize(identifier, transitions)
        super(identifier)
        raise ArgumentError, 'transitions must be specified' unless transitions
        raise ArgumentError, 'transitions must not be an empty Array' if transitions.empty?
        @transitions = transitions.freeze
      end

      # (see DataTimezoneInfo#period_for)
      def period_for(timestamp)
        raise ArgumentError, 'timestamp must be specified' unless timestamp
        raise ArgumentError, 'timestamp must have a specified utc_offset' unless timestamp.utc_offset

        timestamp_value = timestamp.value

        index = find_minimum_transition {|t| t.timestamp_value >= timestamp_value }

        if index
          transition = @transitions[index]

          if transition.timestamp_value == timestamp_value
            # timestamp occurs within the second of the found transition, so is
            # the transition that starts the period.
            start_transition = transition
            end_transition = @transitions[index + 1]
          else
            # timestamp occurs before the second of the found transition, so is
            # the transition that ends the period.
            start_transition = index == 0 ? nil : @transitions[index - 1]
            end_transition = transition
          end
        else
          start_transition = @transitions.last
          end_transition = nil
        end

        TransitionsTimezonePeriod.new(start_transition, end_transition)
      end

      # (see DataTimezoneInfo#periods_for_local)
      def periods_for_local(local_timestamp)
        raise ArgumentError, 'local_timestamp must be specified' unless local_timestamp
        raise ArgumentError, 'local_timestamp must have an unspecified utc_offset' if local_timestamp.utc_offset

        local_timestamp_value = local_timestamp.value
        latest_possible_utc_value = local_timestamp_value + 86400

        # Find the index of the first transition that occurs after a latest
        # possible UTC representation of the local timestamp and then search
        # backwards until an earliest possible UTC representation.
        index = find_minimum_transition {|t| t.timestamp_value >= latest_possible_utc_value }

        # No transitions after latest_possible_utc_value, set to max index + 1
        # to search backwards including the period after the last transition
        index = @transitions.length unless index

        periods_for_local_value(local_timestamp_value, index)
      end

      # Returns an `Array` containing one `Array` of {TimezonePeriod
      # TimezonePeriods} for each of the given local timestamps. Each inner
      # `Array` is ordered by increasing UTC start date and is empty if no
      # periods are found for the corresponding local timestamp.
      #
      # The results are returned in the same order as the given
      # `local_timestamps`, so each result appears at the index of the local
      # timestamp it corresponds to. The local timestamps may be supplied in
      # any order and may contain duplicates.
      #
      # The transition list is traversed once across all the local timestamps
      # rather than separately for each one.
      #
      # @param local_timestamps [Enumerable<Timestamp>] local {Timestamp
      #   Timestamp} instances, each with an unspecified {Timestamp#utc_offset
      #   utc_offset}.
      # @return [Array<Array<TimezonePeriod>>] an `Array` containing the
      #   {TimezonePeriod TimezonePeriods} that could be observed at each local
      #   timestamp specified by `local_timestamps`.
      # @raise [ArgumentError] may be raised if an element of
      #   `local_timestamps` is `nil`, or has a specified
      #   {Timestamp#utc_offset utc_offset}.
      def periods_for_local_batch(local_timestamps)
        # Validate each local timestamp and pair its value with its original
        # index. The pairs are sorted by value for a single, shared traversal
        # of the transition list. The original indices are used to return the
        # results in input order.
        ordered = local_timestamps.each_with_index.map do |local_timestamp, original_index|
          raise ArgumentError, 'local_timestamp must be specified' unless local_timestamp
          raise ArgumentError, 'local_timestamp must have an unspecified utc_offset' if local_timestamp.utc_offset
          [local_timestamp.value, original_index]
        end
        ordered.sort_by!(&:first)

        results = Array.new(ordered.length)

        # Cursor over the transition list. Since local timestamp values are
        # processed in ascending order, this only ever advances, so the
        # transitions are traversed once in total.
        index = 0

        ordered.each do |local_timestamp_value, original_index|
          index = advance_minimum_transition_for_local(local_timestamp_value, index)
          results[original_index] = periods_for_local_value(local_timestamp_value, index)
        end

        results
      end

      # (see DataTimezoneInfo#transitions_up_to)
      def transitions_up_to(to_timestamp, from_timestamp = nil)
        raise ArgumentError, 'to_timestamp must be specified' unless to_timestamp
        raise ArgumentError, 'to_timestamp must have a specified utc_offset' unless to_timestamp.utc_offset

        if from_timestamp
          raise ArgumentError, 'from_timestamp must have a specified utc_offset' unless from_timestamp.utc_offset
          raise ArgumentError, 'to_timestamp must be greater than from_timestamp' if to_timestamp <= from_timestamp
        end

        if from_timestamp
          from_index = find_minimum_transition {|t| transition_on_or_after_timestamp?(t, from_timestamp) }
          return [] unless from_index
        else
          from_index = 0
        end

        to_index = find_minimum_transition {|t| transition_on_or_after_timestamp?(t, to_timestamp) }

        if to_index
          return [] if to_index < 1
          to_index -= 1
        else
          to_index = -1
        end

        @transitions[from_index..to_index]
      end

      private

      # Finds the {TimezonePeriod TimezonePeriods} that could be observed for a
      # local timestamp with the given value, searching backwards from the
      # transition at the given index.
      #
      # @param local_timestamp_value [Integer] the value of the local
      #   timestamp.
      # @param index [Integer] the index of the first transition that occurs at
      #   or after the local timestamp's latest possible UTC representation, or
      #   the length of {#transitions} if no such transition exists.
      # @return [Array<TimezonePeriod>] the {TimezonePeriod TimezonePeriods}
      #   that could be observed at the local timestamp, ordered by increasing
      #   UTC start date.
      def periods_for_local_value(local_timestamp_value, index)
        earliest_possible_utc_value = local_timestamp_value - 86400

        result = []

        index.downto(0) do |i|
          start_transition = i > 0 ? @transitions[i - 1] : nil
          end_transition = @transitions[i]
          offset = start_transition ? start_transition.offset : end_transition.previous_offset
          utc_timestamp_value = local_timestamp_value - offset.observed_utc_offset

          # It is not necessary to compare the sub-seconds because a timestamp
          # is in the period if is >= the start transition (sub-seconds would
          # make == become >) and if it is < the end transition (which
          # sub-seconds cannot affect).
          if (!start_transition || utc_timestamp_value >= start_transition.timestamp_value) && (!end_transition || utc_timestamp_value < end_transition.timestamp_value)
            result << TransitionsTimezonePeriod.new(start_transition, end_transition)
          elsif end_transition && end_transition.timestamp_value < earliest_possible_utc_value
            break
          end
        end

        result.reverse!
      end

      # Advances the cursor over the transition list when processing local
      # timestamps in ascending order. Returns the index of the first
      # transition that occurs at or after the local timestamp's latest
      # possible UTC representation, or the length of {#transitions} if no such
      # transition exists.
      #
      # @param local_timestamp_value [Integer] the value of the local
      #   timestamp.
      # @param index [Integer] a lower bound for the returned index, based on
      #   the local timestamp processed previously (0 for the first local
      #   timestamp).
      # @return [Integer] the index to start searching backwards from.
      def advance_minimum_transition_for_local(local_timestamp_value, index)
        latest_possible_utc_value = local_timestamp_value + 86400

        while index < @transitions.length && @transitions[index].timestamp_value < latest_possible_utc_value
          index += 1
        end

        index
      end

      # Array#bsearch_index was added in Ruby 2.3.0. Use bsearch_index to find
      # transitions if it is available, otherwise use a Ruby implementation.
      if [].respond_to?(:bsearch_index)
        # Performs a binary search on {transitions} to find the index of the
        # earliest transition satisfying a condition.
        #
        # @yield [transition] the caller will be yielded to to test the search
        #   condition.
        # @yieldparam transition [TimezoneTransition] a {TimezoneTransition}
        #   instance from {transitions}.
        # @yieldreturn [Boolean] `true` for the earliest transition that
        #   satisfies the condition and return `true` for all subsequent
        #   transitions. In all other cases, the result of the block must be
        #   `false`.
        # @return [Integer] the index of the earliest transition safisfying
        #   the condition or `nil` if there are no such transitions.
        #
        # :nocov_no_array_bsearch_index:
        def find_minimum_transition(&block)
          @transitions.bsearch_index(&block)
        end
        # :nocov_no_array_bsearch_index:
      else
        # Performs a binary search on {transitions} to find the index of the
        # earliest transition satisfying a condition.
        #
        # @yield [transition] the caller will be yielded to to test the search
        #   condition.
        # @yieldparam transition [TimezoneTransition] a {TimezoneTransition}
        #   instance from {transitions}.
        # @yieldreturn [Boolean] `true` for the earliest transition that
        #   satisfies the condition and return `true` for all subsequent
        #   transitions. In all other cases, the result of the block must be
        #   `false`.
        # @return [Integer] the index of the earliest transition safisfying
        #   the condition or `nil` if there are no such transitions.
        #
        # :nocov_array_bsearch_index:
        def find_minimum_transition
          # A Ruby implementation of the find-minimum mode of Array#bsearch_index.
          low = 0
          high = @transitions.length
          satisfied = false

          while low < high do
            mid = (low + high).div(2)
            if yield @transitions[mid]
              satisfied = true
              high = mid
            else
              low = mid + 1
            end
          end

          satisfied ? low : nil
        end
        # :nocov_array_bsearch_index:
      end

      # Determines if a transition occurs at or after a given {Timestamp},
      # taking the {Timestamp#sub_second sub_second} into consideration.
      #
      # @param transition [TimezoneTransition] the transition to compare.
      # @param timestamp [Timestamp] the timestamp to compare.
      # @return [Boolean] `true` if `transition` occurs at or after `timestamp`,
      #   otherwise `false`.
      def transition_on_or_after_timestamp?(transition, timestamp)
        transition_timestamp_value = transition.timestamp_value
        timestamp_value = timestamp.value
        transition_timestamp_value > timestamp_value || transition_timestamp_value == timestamp_value && timestamp.sub_second == 0
      end
    end
  end
end
