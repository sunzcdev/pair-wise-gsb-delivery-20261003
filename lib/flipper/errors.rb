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

  # Raised when an adapter is asked to resolve a reference to an external
  # feature that no declaration allows it to follow.
  class MissingDependency < Error
    attr_reader :feature_name, :dependency_name

    def initialize(feature_name, dependency_name)
      @feature_name = feature_name.to_s
      @dependency_name = dependency_name.to_s
      super "Feature #{@feature_name.inspect} references external feature #{@dependency_name.inspect}, but no external source is declared for it. Declare the source on the migrating adapter or remove the reference."
    end
  end

  # Raised when attempting to declare a group name that has already been used.
  class DuplicateGroup < Error; end

  # Raised when an invalid value is set to a configuration property
  class InvalidConfigurationValue < Flipper::Error
    def initialize(message = nil)
      default = "Configuration value is not valid."
      super(message || default)
    end
  end
end
