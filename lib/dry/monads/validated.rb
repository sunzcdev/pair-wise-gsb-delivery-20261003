# frozen_string_literal: true

module Dry
  module Monads
    # Validated is similar to Result and represents an outcome of a validation.
    # The difference between Validated and Result is that the former implements
    # `#apply` in a way that accumulates errors. By default the errors are
    # concatenated with `+`, which plays nice with arrays and lists. If your
    # errors don't form a semigroup (e.g. they are plain objects carrying a
    # path and a severity) you can attach an explicit {Validated::Rule} that
    # defines how errors are combined. The rule is preserved by `apply`,
    # `alt_map`, and `List<Validated>#traverse`. Results built with distinct
    # explicit rules can't be combined and will raise {ValidatedRuleMismatchError}.
    # Also, List<Validated>#traverse implicitly uses the rule to wrap errors
    # so that you don't have to do it manually.
    #
    # @example using with List
    #   List::Validated[Valid('London'), Invalid(:name_missing), Invalid(:email_missing)]
    #   # => Invalid(List[:name_missing, :email_missing])
    #
    # @example with valid results
    #   List::Validated[Valid('London'), Valid('John')]
    #   # => Valid(List['London', 'John'])
    #
    # @example with a custom merge rule
    #   rule = Validated.rule { |left, right| left.merge(right) }
    #   rule.invalid(error).apply(rule.invalid(other))
    #
    class Validated
      include ConversionStubs[:to_maybe, :to_result]

      class << self
        # Wraps a value with `Valid`.
        #
        # @overload pure(value)
        #   @param value [Object] value to be wrapped with Valid
        #   @return [Validated::Valid]
        #
        # @overload pure(&block)
        #   @param block [Object] block to be wrapped with Valid
        #   @return [Validated::Valid]
        #
        def pure(value = Undefined, &block)
          Valid.new(Undefined.default(value, block))
        end

        # Builds an explicit error merge rule.
        # Every block invocation receives two accumulated errors and must
        # return their combination.
        #
        # @example
        #   rule = Validated.rule { |left, right| left.merge(right) }
        #
        # @param name [String, nil] optional name used in error messages
        # @param wrap [#call] lifts a single error into the accumulator used
        #   by the rule. Defaults to the identity, meaning the values passed
        #   to {Invalid} are expected to already be accumulators.
        # @yieldparam left [Object] previously accumulated errors
        # @yieldparam right [Object] newly accumulated errors
        # @return [Validated::Rule]
        def rule(name = Undefined, wrap: Undefined, &combine)
          wrap_proc = Undefined.default(wrap, nil)
          Rule.new(combine, Undefined.default(name, nil), wrap_proc)
        end
      end

      # Returns self.
      #
      # @return [Validated::Valid, Validated::Invalid]
      def to_monad = self

      # The rule used to merge errors when this result meets another
      # Validated in `apply` or `List#traverse`.
      #
      # @return [Validated::Rule]
      attr_reader :rule

      # Bind/flat_map is not implemented
      #
      def bind(...)
        # See https://typelevel.org/cats/datatypes/validated.html for details on why
        raise NotImplementedError,
          "Validated is not a monad because it would violate the monad laws"
      end

      private

      # Resolves the rule to use for combining two validated values.
      # The default rule stands for "no rule configured" and yields to any
      # explicit rule, while two distinct explicit rules are rejected.
      #
      # @param other [Validated]
      # @return [Validated::Rule]
      def merge_rule(other)
        other_rule = other.rule

        if Rule.default?(rule)
          other_rule
        elsif Rule.default?(other_rule) || rule.equal?(other_rule)
          rule
        else
          raise ValidatedRuleMismatchError.new(rule, other_rule)
        end
      end

      # Valid result
      #
      class Valid < Validated
        include Dry::Equalizer(:value!)

        def initialize(value, rule: Rule::DEFAULT)
          super()

          @value = value
          @rule = rule
        end

        # Extracts the value
        #
        # @return [Object]
        def value! = @value

        # Applies another Valid to the stored function.
        # The merge rule is carried over to the result so that subsequent
        # combinations keep using it.
        #
        # @overload apply(val)
        #   @example
        #     Validated.pure { |x| x + 1 }.apply(Valid(2)) # => Valid(3)
        #
        #   @param val [Validated::Valid,Validated::Invalid]
        #   @return [Validated::Valid,Validated::Invalid]
        #
        # @overload apply
        #   @example
        #     Validated.pure { |x| x + 1 }.apply { Valid(4) } # => Valid(5)
        #
        #   @yieldreturn [Validated::Valid,Validated::Invalid]
        #   @return [Validated::Valid,Validated::Invalid]
        #
        def apply(val = Undefined, &block)
          other = Undefined.default(val, &block)
          other.fmap(Curry.(value!)).with_rule(merge_rule(other))
        end

        # Lifts a block/proc over Valid
        #
        # @overload fmap(proc)
        #   @param proc [#call]
        #   @return [Validated::Valid]
        #
        # @overload fmap
        #   @param block [Proc]
        #   @return [Validated::Valid]
        #
        def fmap(proc = Undefined, &block)
          f = Undefined.default(proc, block)
          self.class.new(f.(value!), rule: rule)
        end

        # Ignores values and returns self, see {Invalid#alt_map}
        #
        # @return [Validated::Valid]
        def alt_map(_ = nil, &) = self

        # Ignores arguments, returns self
        #
        # @return [Validated::Valid]
        def or(_ = nil, &) = self

        # @return [String]
        def inspect
          if Unit.equal?(@value)
            "Valid()"
          else
            "Valid(#{@value.inspect})"
          end
        end
        alias_method :to_s, :inspect

        # @param other [Object]
        # @return [Boolean]
        def ===(other)
          other.instance_of?(self.class) && value! === other.value!
        end

        # @api private
        def with_rule(new_rule)
          if new_rule.equal?(rule)
            self
          else
            self.class.new(value!, rule: new_rule)
          end
        end
      end

      # Invalid result
      #
      class Invalid < Validated
        # The value stored inside
        #
        # @return [Object]
        attr_reader :error

        # Line where the value was constructed
        #
        # @return [String]
        # @api public
        attr_reader :trace

        include Dry::Equalizer(:error)

        def initialize(error, trace = RightBiased::Left.trace_caller, rule: Rule::DEFAULT)
          super()

          @error = error
          @trace = trace
          @rule = rule
        end

        # Collects errors (ignores valid results).
        # Errors are combined with the attached merge {Rule}, which is also
        # carried over to the result. Combining values created with different
        # explicit rules raises {ValidatedRuleMismatchError}.
        #
        # @overload apply(val)
        #   @param val [Validated::Valid,Validated::Invalid]
        #   @return [Validated::Invalid]
        #
        # @overload apply
        #   @yieldreturn [Validated::Valid,Validated::Invalid]
        #   @return [Validated::Invalid]
        #
        def apply(val = Undefined, &block)
          other = Undefined.default(val, &block)
          combined_rule = merge_rule(other)

          if other.is_a?(Invalid)
            self.class.new(
              combined_rule.call(@error, other.error),
              RightBiased::Left.trace_caller,
              rule: combined_rule
            )
          else
            with_rule(combined_rule)
          end
        end

        # Lifts a block/proc over Invalid.
        # The merge rule is preserved so that transforming the error value
        # doesn't fall back to the default rule.
        #
        # @overload alt_map(proc)
        #   @param proc [#call]
        #   @return [Validated::Invalid]
        #
        # @overload alt_map
        #   @param block [Proc]
        #   @return [Validated::Invalid]
        #
        def alt_map(proc = Undefined, &block)
          f = Undefined.default(proc, block)
          self.class.new(
            f.(error),
            RightBiased::Left.trace_caller,
            rule: rule
          )
        end

        # Ignores the passed argument and returns self
        #
        # @return [Validated::Invalid]
        def fmap(_ = nil, &) = self

        # Yields the given callable and returns the result
        #
        # @overload or(proc)
        #   @param proc [#call]
        #   @return [Object]
        #
        # @overload or
        #   @param block [Proc]
        #   @return [Object]
        #
        def or(proc = Undefined, &block)
          Undefined.default(proc, block).call
        end

        # @return [String]
        def inspect = "Invalid(#{@error.inspect})"
        alias_method :to_s, :inspect

        # @param other [Object]
        # @return [Boolean]
        def ===(other)
          other.instance_of?(self.class) && error === other.error
        end

        # @api private
        def with_rule(new_rule)
          if new_rule.equal?(rule)
            self
          else
            self.class.new(error, trace, rule: new_rule)
          end
        end
      end

      # An explicit rule defining how errors of {Invalid} values are combined.
      # See {Validated.rule} for building one.

      # Mixin with Validated constructors
      #
      module Mixin
        # Successful validation result
        # @see Dry::Monads::Validated::Valid
        Valid = Valid

        # Unsuccessful validation result
        # @see Dry::Monads::Validated::Invalid
        Invalid = Invalid

        # Actual constructor methods
        #
        module Constructors
          # Valid constructor
          #
          # @overload Valid(value)
          #   @param value [Object]
          #   @return [Valdated::Valid]
          #
          # @overload Valid(&block)
          #   @param block [Proc]
          #   @return [Valdated::Valid]
          #
          def Valid(value = Undefined, &block)
            v = Undefined.default(value, block)
            raise ArgumentError, "No value given" if !value.nil? && v.nil?

            Valid.new(v)
          end

          # Invalid constructor
          #
          # @overload Invalid(value)
          #   @param value [Object]
          #   @return [Valdated::Invalid]
          #
          # @overload Invalid(&block)
          #   @param block [Proc]
          #   @return [Valdated::Invalid]
          #
          def Invalid(value = Undefined, &block)
            v = Undefined.default(value, block)
            raise ArgumentError, "No value given" if !value.nil? && v.nil?

            Invalid.new(v, RightBiased::Left.trace_caller)
          end
        end

        include Constructors
      end
    end

    extend Validated::Mixin::Constructors

    # @see Validated::Valid
    Valid = Validated::Valid
    # @see Validated::Invalid
    Invalid = Validated::Invalid

    class Result
      class Success < Result
        # Transforms to Validated
        #
        # @return [Validated::Valid]
        def to_validated = Validated::Valid.new(value!)
      end

      class Failure < Result
        # Transforms to Validated
        #
        # @return [Validated::Invalid]
        def to_validated = Validated::Invalid.new(failure, trace)
      end
    end

    require "dry/monads/registry"
    register_mixin(:validated, Validated::Mixin)
  end
end
