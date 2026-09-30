# frozen_string_literal: true

module Dry
  module Monads
    class Validated
      # An explicit rule defining how errors of {Invalid} values are combined.
      # Build one with {Validated.rule}. The rule object's identity is used
      # to make sure values carrying different rules are never merged silently.
      class Rule
        # @return [#call] accumulates two errors into one
        attr_reader :combine

        # @return [#call, nil] lifts a single error into the accumulator
        attr_reader :lift_proc

        # @return [String, nil] name used in mismatch messages
        attr_reader :name

        # @api private
        def initialize(combine, name = nil, lift_proc = nil)
          @combine = combine
          @name = name
          @lift_proc = lift_proc
        end

        # The rule implicitly used when no explicit rule is given.
        # It concatenates errors with `+` and wraps a single error with a
        # {List} so that List<Validated>#traverse works out of the box.
        DEFAULT = new(
          ->(left, right) { left + right },
          "default",
          ->(error) { List::Validated.pure(error) }
        ).freeze

        # Combines two accumulated errors.
        #
        # @param left [Object]
        # @param right [Object]
        # @return [Object]
        def call(left, right) = @combine.call(left, right)

        # Lifts a single error into the accumulator understood by {#call}.
        # When no lifting proc was given the error is returned as-is, meaning
        # it's expected to already be an accumulator.
        #
        # @param error [Object]
        # @return [Object]
        def lift(error)
          @lift_proc ? @lift_proc.call(error) : error
        end

        # Whether the given rule is the implicit default rule.
        #
        # @param rule [Rule]
        # @return [Boolean]
        def self.default?(rule) = rule.equal?(DEFAULT)

        # Builds a {Validated::Invalid} carrying this rule.
        #
        # @param error [Object]
        # @return [Validated::Invalid]
        def invalid(error)
          Validated::Invalid.new(error, RightBiased::Left.trace_caller, rule: self)
        end

        # Builds a {Validated::Valid} carrying this rule.
        #
        # @param value [Object]
        # @return [Validated::Valid]
        def valid(value)
          Validated::Valid.new(value, rule: self)
        end

        # @return [String]
        def inspect
          name ? "#{self.class.name}(#{name})" : super
        end
      end
    end
  end
end
