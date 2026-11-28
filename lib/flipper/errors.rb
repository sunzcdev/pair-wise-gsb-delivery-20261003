module Flipper
  # Top level error that all other errors inherit from.
  class Error < StandardError; end

  # Raised when gate can not be found for an actor.
  class GateNotFound < Error
    def initialize(actor)
      super "Could not find gate for #{actor.inspect}"
    end
  end

  # Raised when writing a feature whose key starts with
  # Flipper::RESERVED_KEY_PREFIX. An ArgumentError, like the adapters' other
  # reserved name errors.
  class ReservedKeyError < ArgumentError
    attr_reader :key

    def initialize(key)
      @key = key.to_s
      super "#{@key.inspect} is reserved for Flipper's internal use and cannot be used as a feature name"
    end
  end

  # Raised when attempting to declare a group name that has already been used.
  class DuplicateGroup < Error; end

  # Raised when a replacement of one member of a set gate with another cannot
  # be applied because the members are not what the caller observed.
  #
  # This is the deterministic outcome of two operations racing on the same
  # members: the one whose expected members still match applies the change,
  # and the other gets this error instead of overwriting the winner or
  # applying a different change to members it never saw. Nothing has been
  # written when this is raised, so re-reading the members and retrying is
  # safe.
  #
  # Also raised when the member that should leave is no longer enabled and
  # the member that should join is not enabled either, because there is
  # nothing to replace and adding the new member would grow the set.
  class ActorSetConflict < Error
    # Public: The key of the feature whose members did not match.
    attr_reader :feature_key

    # Public: The Set of member values the caller said it observed, or nil
    # when the caller did not supply an expectation.
    attr_reader :expected

    # Public: The Set of member values the adapter reported at the time.
    attr_reader :actual

    def initialize(message = nil, feature_key: nil, expected: nil, actual: nil)
      @feature_key = feature_key
      @expected = expected
      @actual = actual
      super(message || default_message)
    end

    # Public: Was this caused by a concurrent change the caller can re-read
    # and retry against?
    def retryable?
      true
    end

    private

    def default_message
      expected_values = expected ? expected.to_a.sort.inspect : "unknown"
      actual_values = actual ? actual.to_a.sort.inspect : "unknown"
      "Members for feature #{feature_key.inspect} changed while replacing a member. " \
        "Expected #{expected_values}, found #{actual_values}. Re-read the members and retry."
    end
  end

  # Raised when an invalid value is set to a configuration property
  class InvalidConfigurationValue < Flipper::Error
    def initialize(message = nil)
      default = "Configuration value is not valid."
      super(message || default)
    end
  end
end
