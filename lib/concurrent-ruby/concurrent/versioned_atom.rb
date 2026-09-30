require 'concurrent/collection/copy_on_notify_observer_set'
require 'concurrent/concern/observable'
require 'concurrent/synchronization/object'

module Concurrent

  # A {VersionedAtom} is an {Atom} that pairs its value with an internal,
  # monotonically increasing version. Every read returns the value *and* the
  # version that were current at the same instant, and a conditional update
  # only commits when both match the supplied snapshot.
  #
  # Unlike a bare {Atom}, a stale snapshot can never overwrite a later state
  # merely because the value happened to change back to what it was before.
  # If the value changes A -> B -> A between the read and the conditional
  # write, the observed value is again `A` but the version has advanced, so
  # the old `(A, old_version)` snapshot is rejected.
  #
  # The version is owned by the atom itself. Callers can read it and pass it
  # back to {#compare_and_set}, but they cannot set it or roll it back. The
  # version advances exactly once per successful commit, including a commit
  # that stores a value equal to the current one. A failed validation, a
  # raised validator, or a lost compare-and-set race does not advance the
  # version.
  #
  # ## Observation
  #
  # As with {Atom}, observers are notified after every committed change. The
  # observer receives five arguments: `time`, `old_value`, `new_value`,
  # `old_version`, and `new_version`. The versions describe the state
  # immediately before and after the commit that actually happened, even when
  # {#swap} had to retry after losing a race.
  #
  # @!macro thread_safe_variable_comparison
  #
  # @see Concurrent::Atom
  class VersionedAtom < Synchronization::Object
    include Concern::Observable

    safe_initialization!
    attr_atomic(:state)
    private :state, :state=, :swap_state, :compare_and_set_state, :update_state

    # Create a new versioned atom with the given initial value.
    #
    # @param [Object] value The initial value.
    # @param [Hash] opts The options used to configure the atom.
    # @option opts [Proc] :validator (nil) Optional proc used to validate new
    #   values. It must accept one and only one argument which will be the
    #   intended new value. The validator will return true if the new value
    #   is acceptable else return false (preferably) or raise an exception.
    #
    # @raise [ArgumentError] if the validator is not a `Proc` (when given)
    def initialize(value, opts = {})
      super()
      @Validator     = opts.fetch(:validator, -> v { true })
      self.observers = Collection::CopyOnNotifyObserverSet.new
      self.state     = state_with(value, 0)
    end

    # The current value of the atom.
    #
    # @return [Object] The current value.
    def value
      state[0]
    end
    alias_method :deref, :value

    # The version of the current state.
    #
    # The initial state has version zero and the version is incremented
    # exactly once for every successful commit.
    #
    # @return [Integer] The current version.
    def version
      state[1]
    end

    # Returns the value and version current at the same instant.
    #
    # @return [Array(Object, Integer)] A frozen `[value, version]` pair.
    def snapshot
      state
    end

    # Atomically swaps the value of the atom using the given block.
    #
    # Internally this reads a fresh `[value, version]` snapshot, applies the
    # block to the value, and attempts to commit it. Since another thread may
    # have committed in the intervening time, it may have to retry, and does
    # so in a spin loop, rereading both the value and the version on every
    # attempt. The block might be called multiple times and thus must be free
    # of side effects.
    #
    # @note The given block may be called multiple times, and thus should be
    #   free of side effects.
    #
    # @param [Object] args Zero or more arguments passed to the block.
    #
    # @yield [value, args] Calculates a new value for the atom based on the
    #   current value and any supplied arguments.
    # @yieldparam value [Object] The current value of the atom.
    # @yieldparam args [Object] All arguments passed to the function, in order.
    # @yieldreturn [Object] The intended new value of the atom.
    #
    # @return [Object] The final value of the atom after all attempts and
    #   validations are complete.
    #
    # @raise [ArgumentError] When no block is given.
    def swap(*args)
      raise ArgumentError.new('no block given') unless block_given?

      loop do
        old_state = state
        old_value = old_state[0]
        new_value = yield(old_value, *args)
        begin
          break old_value unless valid?(new_value)
          committed_state = try_commit(old_state, new_value)
          break committed_state[0] if committed_state
        rescue
          break old_value
        end
      end
    end

    # Atomically sets the value to `new_value` if and only if the current
    # value is identical to `old_value`, the current version is identical to
    # `old_version`, and the new value passes the (optional) validator given
    # at construction.
    #
    # @param [Object] old_value The expected current value.
    # @param [Integer] old_version The version of the snapshot the expected
    #   current value was read from.
    # @param [Object] new_value The intended new value.
    #
    # @return [Boolean] True if the value was changed else false.
    def compare_and_set(old_value, old_version, new_value)
      return false unless valid?(new_value)

      old_state = state
      return false unless old_state[0].equal?(old_value) && old_state[1] == old_version

      !try_commit(old_state, new_value).nil?
    end

    # Atomically sets the value to `new_value` without regard for the current
    # value or version, so long as the new value validates against the
    # (optional) validator given at construction. A successful reset still
    # advances the version.
    #
    # @param [Object] new_value The intended new value.
    #
    # @return [Object] The final value of the atom after all operations and
    #   validations are complete.
    def reset(new_value)
      old_value = value
      return old_value unless valid?(new_value)

      loop do
        old_state = state
        committed_state = try_commit(old_state, new_value)
        return committed_state[0] if committed_state
      end
    end

    private

    # Attempts a single commit from `old_state` to `new_value`.
    #
    # @param [Array] old_state The frozen `[value, version]` snapshot the
    #   attempt is based on.
    # @param [Object] new_value The already validated intended new value.
    #
    # @return [Array, nil] The frozen committed `[value, version]` state on
    #   success, or `nil` if another thread committed first.
    def try_commit(old_state, new_value)
      new_state = state_with(new_value, old_state[1] + 1)
      if compare_and_set_state(old_state, new_state)
        observers.notify_observers(
          Time.now, old_state[0], new_value, old_state[1], new_state[1]
        )
        new_state
      end
    end

    # Builds an immutable internal state pair.
    #
    # @param [Object] value The value.
    # @param [Integer] version The version.
    # @return [Array] The frozen `[value, version]` pair.
    def state_with(value, version)
      [value, version].freeze
    end

    # Is the new value valid?
    #
    # @param [Object] new_value The intended new value.
    # @return [Boolean] false if the validator function returns false or raises
    #   an exception else true
    def valid?(new_value)
      @Validator.call(new_value)
    rescue
      false
    end
  end
end
