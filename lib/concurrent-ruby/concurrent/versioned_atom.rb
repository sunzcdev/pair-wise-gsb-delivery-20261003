require 'concurrent/collection/copy_on_notify_observer_set'
require 'concurrent/concern/observable'
require 'concurrent/synchronization/object'

module Concurrent

  # A versioned variant of {Atom}.
  #
  # Reads return an immutable snapshot containing the value and its version.
  # Conditional updates require the exact snapshot returned by a previous read.
  # The version is incremented only when a new value is successfully committed.
  class VersionedAtom < Synchronization::Object
    include Concern::Observable

    UNDEFINED = Object.new
    private_constant :UNDEFINED

    safe_initialization!
    attr_atomic(:snapshot)
    private :snapshot=, :swap_snapshot, :compare_and_set_snapshot, :update_snapshot
    public :snapshot
    alias_method :read, :snapshot
    alias_method :get, :snapshot
    alias_method :deref, :snapshot

    # @!method snapshot
    #   The current value and version as an immutable two-element array.
    #
    #   @return [Array(Object, Integer)] The current value and version.

    # Create a new versioned atom with the given initial value.
    #
    # @param [Object] value The initial value.
    # @param [Hash] opts The options used to configure the atom.
    # @option opts [Proc] :validator Optional proc used to validate new values.
    def initialize(value, opts = {})
      super()
      @Validator     = opts.fetch(:validator, -> v { true })
      self.observers = Collection::CopyOnNotifyObserverSet.new
      self.snapshot  = immutable_snapshot(value, 0)
    end

    # @return [Object] The current value.
    def value
      snapshot[0]
    end

    # @return [Integer] The current version.
    def version
      snapshot[1]
    end

    # Atomically swaps the value using the given block. The block may be called
    # multiple times after losing a compare-and-set race. Each retry receives the
    # latest committed value.
    #
    # @param [Object] args Zero or more arguments passed to the block.
    # @yield [value, args] Calculates a new value.
    # @return [Object] The final value after all operations and validations.
    # @raise [ArgumentError] When no block is given.
    def swap(*args)
      raise ArgumentError.new('no block given') unless block_given?

      loop do
        old_snapshot = snapshot
        new_value = yield(old_snapshot[0], *args)

        begin
          break old_snapshot[0] unless valid?(new_value)
          break new_value if compare_and_set(old_snapshot, new_value)
        rescue
          break old_snapshot[0]
        end
      end
    end

    # Sets the new value if and only if the supplied snapshot, or supplied value
    # and version, still identifies the current state and the new value
    # validates successfully.
    #
    # @param [Array(Object, Integer)] old_snapshot A snapshot returned by #snapshot.
    # @param [Object] old_value A value returned with a version by #snapshot.
    # @param [Integer] old_version A version returned with a value by #snapshot.
    # @param [Object] new_value The intended new value.
    # @return [Boolean] True when the new value was committed.
    def compare_and_set(first, second, third = UNDEFINED)
      if third.equal?(UNDEFINED)
        old_snapshot = first
        new_value = second
      else
        current_snapshot = snapshot
        return false unless same_value?(current_snapshot[0], first)
        return false unless current_snapshot[1] == second

        old_snapshot = current_snapshot
        new_value = third
      end

      current_snapshot = snapshot
      return false unless old_snapshot.equal?(current_snapshot)
      return false unless valid?(new_value)

      new_snapshot = immutable_snapshot(new_value, current_snapshot[1] + 1)
      return false unless compare_and_set_snapshot(current_snapshot, new_snapshot)

      observers.notify_observers(
        Time.now,
        current_snapshot[0],
        new_value,
        current_snapshot[1],
        new_snapshot[1]
      )
      true
    end

    # Unconditionally sets a validated new value and increments the version.
    #
    # @param [Object] new_value The intended new value.
    # @return [Object] The final value after validation.
    def reset(new_value)
      loop do
        old_snapshot = snapshot
        return old_snapshot[0] unless valid?(new_value)

        new_snapshot = immutable_snapshot(new_value, old_snapshot[1] + 1)
        next unless compare_and_set_snapshot(old_snapshot, new_snapshot)

        observers.notify_observers(
          Time.now,
          old_snapshot[0],
          new_value,
          old_snapshot[1],
          new_snapshot[1]
        )
        return new_value
      end
    end

    private

    def immutable_snapshot(value, version)
      [value, version].freeze
    end

    def valid?(new_value)
      @Validator.call(new_value)
    rescue
      false
    end

    def same_value?(current_value, expected_value)
      if expected_value.is_a?(Numeric)
        current_value.is_a?(Numeric) && current_value == expected_value
      else
        current_value.equal?(expected_value)
      end
    end
  end
end
