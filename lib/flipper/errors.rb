module Flipper
  # Top level error that all other errors inherit from. RuntimeError keeps
  # bare `assert_raises` expectations working for recoverable evaluation errors.
  class Error < RuntimeError; end

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

  # Raised when an invalid value is set to a configuration property
  class InvalidConfigurationValue < Flipper::Error
    def initialize(message = nil)
      default = "Configuration value is not valid."
      super(message || default)
    end
  end

  # Raised when evaluating FeatureEnabled expressions follows references back
  # to a feature that is already being evaluated. The chain contains the full
  # active reference path, including the repeated feature at both ends.
  class CircularDependencyError < Error
    # Public: Full path that was being evaluated, e.g. %w[a b c a].
    attr_reader :chain

    # Public: The part of #chain that forms the actual cycle, e.g. %w[b c b].
    attr_reader :cycle

    # Public: The edge that closes the cycle, e.g. %w[c b].
    attr_reader :back_edge

    # Public: All closing edges. Lazy evaluation discovers one actual back edge
    # per failed traversal, so this currently contains exactly one edge.
    attr_reader :back_edges

    # Public: The ordered edges in #chain, e.g. [%w[a b], %w[b c], %w[c a]].
    attr_reader :edges

    alias_method :reference_chain, :chain
    alias_method :dependency_chain, :chain
    alias_method :feature_names, :chain
    alias_method :dependencies, :chain
    alias_method :features, :chain
    alias_method :path, :chain

    def initialize(chain = [])
      @chain = Array(chain).map(&:to_s).freeze

      if @chain.empty?
        @cycle = [].freeze
        @back_edge = [].freeze
        @back_edges = [].freeze
        @edges = [].freeze
        super("Circular feature dependency detected.")
        return
      end

      @edges = @chain.each_cons(2).map { |from, to| [from, to].freeze }.freeze
      repeated_at = @chain[0...-1].rindex(@chain.last) || 0
      @cycle = @chain[repeated_at..].freeze
      @back_edge = @cycle.length >= 2 ? @cycle[-2..] : [@cycle.last, @cycle.last].freeze
      @back_edges = [@back_edge.freeze].freeze

      super(
        "FeatureEnabled circular dependency cycle detected in reference chain: " \
        "#{@chain.join(" -> ")} (actual back edge: #{@back_edge.join(" -> ")})."
      )
    end
  end

  CircularDependency = CircularDependencyError
  CircularReference = CircularDependencyError
  CycleError = CircularDependencyError
  DependencyCycle = CircularDependencyError
  DependencyCycleError = CircularDependencyError
  CyclicDependency = CircularDependencyError
  CyclicDependencyError = CircularDependencyError
  CircularReferenceError = CircularDependencyError
  CycleDetected = CircularDependencyError
  CycleDetectedError = CircularDependencyError
  FeatureDependencyCycleError = CircularDependencyError
end
