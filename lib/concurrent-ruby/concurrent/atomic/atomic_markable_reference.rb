require 'concurrent/errors'
require 'concurrent/synchronization/object'

module Concurrent
  # An atomic reference which maintains an object reference along with a mark bit
  # that can be updated atomically.
  #
  # The reference and the mark are stored together as one immutable pair inside
  # a single {Concurrent::AtomicReference}. Every read of the pair is therefore
  # one atomic snapshot, and every update is a single compare-and-set of the
  # whole pair. This holds identically on every supported platform (CRuby with
  # or without the C extension, JRuby, TruffleRuby): the platform-specific
  # {Concurrent::AtomicReference} implementations all provide the same
  # compare-and-set contract, so no behavior here depends on which runtime is
  # in use.
  #
  # @note Comparison protocol (unchanged from the original API): the expected
  #   value is compared to the current value by *value equality* (`==`) when
  #   the expected value is a `Numeric`, and by *identity* (`equal?`) for all
  #   other objects. Marks are compared with `==` (use `true`/`false`).
  #
  # @see http://docs.oracle.com/javase/7/docs/api/java/util/concurrent/atomic/AtomicMarkableReference.html
  #   java.util.concurrent.atomic.AtomicMarkableReference
  class AtomicMarkableReference < ::Concurrent::Synchronization::Object

    attr_atomic(:reference)
    private :reference, :reference=, :swap_reference, :compare_and_set_reference, :update_reference

    def initialize(value = nil, mark = false)
      super()
      self.reference = immutable_array(value, mark)
    end

    # Atomically sets the value and mark to the given updated value and
    # mark given both:
    #   - the current value == the expected value &&
    #   - the current mark == the expected mark
    #
    # @param [Object] expected_val the expected value
    # @param [Object] new_val the new value
    # @param [Boolean] expected_mark the expected mark
    # @param [Boolean] new_mark the new mark
    #
    # @return [Boolean] `true` if successful. A `false` return indicates
    # that the actual value was not equal to the expected value or the
    # actual mark was not equal to the expected mark
    def compare_and_set(expected_val, new_val, expected_mark, new_mark)
      compare_and_set_observed(expected_val, new_val, expected_mark, new_mark)[0]
    end

    alias_method :compare_and_swap, :compare_and_set

    # Like {#compare_and_set}, but additionally reports the reference/mark
    # pair observed by this operation, so the caller learns the outcome *and*
    # a consistent snapshot from a single atomic operation — instead of
    # getting `false` and then issuing separate reads that could be
    # interleaved with other updates and stitched into a combination that
    # never actually existed.
    #
    # The returned pair is always a real one: it is the content of the
    # atomic reference at a single point in time during this call.
    # Specifically:
    #   - on success, the pair just installed (`new_val`, `new_mark`);
    #   - on failure, the pair read by one atomic snapshot (if the
    #     compare-and-set itself lost a race, the pair is re-read once,
    #     again as a single atomic snapshot of the whole pair).
    #
    # @param [Object] expected_val the expected value
    # @param [Object] new_val the new value
    # @param [Boolean] expected_mark the expected mark
    # @param [Boolean] new_mark the new mark
    #
    # @return [Array(Boolean, Object, Boolean)] a frozen triple of
    #   `success`, `observed_value`, `observed_mark`
    def compare_and_set_observed(expected_val, new_val, expected_mark, new_mark)
      # One atomic snapshot of the whole [value, mark] pair.
      current             = reference
      curr_val, curr_mark = current

      unless expected_match? expected_val, curr_val, expected_mark, curr_mark
        return immutable_array(false, curr_val, curr_mark)
      end

      prospect = immutable_array(new_val, new_mark)

      if compare_and_set_reference current, prospect
        immutable_array(true, new_val, new_mark)
      else
        # Lost a race against a concurrent update. Re-read the pair with one
        # atomic snapshot so the reported combination really existed.
        observed_val, observed_mark = reference
        immutable_array(false, observed_val, observed_mark)
      end
    end

    # Gets the current reference and marked values.
    #
    # @return [Array] the current reference and marked values
    def get
      reference
    end

    # Gets the current value of the reference
    #
    # @return [Object] the current value of the reference
    def value
      reference[0]
    end

    # Gets the current marked value
    #
    # @return [Boolean] the current marked value
    def mark
      reference[1]
    end

    alias_method :marked?, :mark

    # _Unconditionally_ sets to the given value of both the reference and
    # the mark.
    #
    # @param [Object] new_val the new value
    # @param [Boolean] new_mark the new mark
    #
    # @return [Array] both the new value and the new mark
    def set(new_val, new_mark)
      self.reference = immutable_array(new_val, new_mark)
    end

    # Pass the current value and marked state to the given block, replacing it
    # with the block's results. May retry if the value changes during the
    # block's execution.
    #
    # @note Under contention the block may be invoked *multiple times* (once
    #   per retry), and there is no upper bound on the number of invocations.
    #   The block must therefore be free of side effects, or its side effects
    #   must be idempotent — it must be safe to run the block any number of
    #   times. If you need the block to run at most once, use {#try_update},
    #   {#try_update!}, or {#try_update_observed} instead.
    #
    # @yield [Object] Calculate a new value and marked state for the atomic
    #   reference using given (old) value and (old) marked
    # @yieldparam [Object] old_val the starting value of the atomic reference
    # @yieldparam [Boolean] old_mark the starting state of marked
    #
    # @return [Array] the new value and new mark
    def update
      loop do
        old_val, old_mark = reference
        new_val, new_mark = yield old_val, old_mark

        if compare_and_set old_val, new_val, old_mark, new_mark
          return immutable_array(new_val, new_mark)
        end
      end
    end

    # Pass the current value to the given block, replacing it
    # with the block's result. Raise an exception if the update
    # fails.
    #
    # @note The block is invoked *exactly once* — this method never retries.
    #   Side effects inside the block therefore happen at most once, but they
    #   are *not* rolled back if the update fails.
    #
    # @yield [Object] Calculate a new value and marked state for the atomic
    #   reference using given (old) value and (old) marked
    # @yieldparam [Object] old_val the starting value of the atomic reference
    # @yieldparam [Boolean] old_mark the starting state of marked
    #
    # @return [Array] the new value and marked state
    #
    # @raise [Concurrent::ConcurrentUpdateError] if the update fails
    def try_update!
      old_val, old_mark = reference
      new_val, new_mark = yield old_val, old_mark

      unless compare_and_set old_val, new_val, old_mark, new_mark
        fail ::Concurrent::ConcurrentUpdateError,
             'AtomicMarkableReference: Update failed due to race condition.',
             'Note: If you would like to guarantee an update, please use ' +
                 'the `AtomicMarkableReference#update` method.'
      end

      immutable_array(new_val, new_mark)
    end

    # Pass the current value to the given block, replacing it with the
    # block's result. Simply return nil if update fails.
    #
    # @note The block is invoked *exactly once* — this method never retries.
    #   Side effects inside the block therefore happen at most once, but they
    #   are *not* rolled back if the update fails.
    #
    # @yield [Object] Calculate a new value and marked state for the atomic
    #   reference using given (old) value and (old) marked
    # @yieldparam [Object] old_val the starting value of the atomic reference
    # @yieldparam [Boolean] old_mark the starting state of marked
    #
    # @return [Array] the new value and marked state, or nil if
    # the update failed
    def try_update
      success, new_val, new_mark =
        try_update_observed { |old_val, old_mark| yield old_val, old_mark }

      immutable_array(new_val, new_mark) if success
    end

    # Pass the current value and marked state to the given block, replacing
    # them with the block's result, and report the outcome together with the
    # observed reference/mark pair — all from a single atomic operation.
    #
    # Unlike {#try_update}, a failure does not return a bare `nil` that would
    # have to be followed by separate {#value}/{#mark} reads (which could be
    # interleaved with other updates and stitched into a combination that
    # never existed). The returned pair is always a real one:
    #   - on success, the pair just installed by this call;
    #   - on failure, the pair read by one atomic snapshot of the whole
    #     `[value, mark]` pair (re-read once if the compare-and-set itself
    #     lost a race).
    #
    # @note The block is invoked *exactly once* — this method never retries.
    #   Side effects inside the block therefore happen at most once, but they
    #   are *not* rolled back if the update fails.
    #
    # @yield [Object] Calculate a new value and marked state for the atomic
    #   reference using given (old) value and (old) marked
    # @yieldparam [Object] old_val the starting value of the atomic reference
    # @yieldparam [Boolean] old_mark the starting state of marked
    #
    # @return [Array(Boolean, Object, Boolean)] a frozen triple of
    #   `success`, `observed_value`, `observed_mark`
    def try_update_observed
      old_val, old_mark = reference
      new_val, new_mark = yield old_val, old_mark

      compare_and_set_observed old_val, new_val, old_mark, new_mark
    end

    private

    # The comparison protocol shared by all update operations: marks are
    # compared with `==`; values are compared by numerical equality (`==`)
    # when the expected value is a Numeric, and by object identity
    # (`equal?`) otherwise.
    def expected_match?(expected_val, curr_val, expected_mark, curr_mark)
      # Ensure that that the expected marks match.
      return false unless expected_mark == curr_mark

      if expected_val.is_a? Numeric
        # If the object is a numeric, we need to ensure we are comparing
        # the numerical values
        expected_val == curr_val
      else
        # Otherwise, we need to ensure we are comparing the object identity.
        # Theoretically, this could be incorrect if a user monkey-patched
        # `Object#equal?`, but they should know that they are playing with
        # fire at that point.
        expected_val.equal? curr_val
      end
    end

    def immutable_array(*args)
      args.freeze
    end
  end
end
