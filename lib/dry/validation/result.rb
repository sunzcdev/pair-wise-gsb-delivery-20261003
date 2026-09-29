# frozen_string_literal: true

require "concurrent/map"

require "dry/validation/constants"

module Dry
  module Validation
    # Result objects are returned by contracts
    #
    # @api public
    class Result
      include ::Dry::Equalizer(:schema_result, :context, :errors, inspect: false)

      # Build a new result
      #
      # @param [Dry::Schema::Result] schema_result
      #
      # @api private
      def self.new(schema_result, context = ::Concurrent::Map.new, options = EMPTY_HASH, **rest)
        result = super
        yield(result) if block_given?
        result.freeze
      end

      # Context that's shared between rules
      #
      # @return [Concurrent::Map]
      #
      # @api public
      attr_reader :context

      # Result from contract's schema
      #
      # @return [Dry::Schema::Result]
      #
      # @api private
      attr_reader :schema_result

      # Result options
      #
      # @return [Hash]
      #
      # @api private
      attr_reader :options

      # The contract that produced this result (used to re-validate fixed input)
      #
      # @return [Contract, nil]
      #
      # @api private
      attr_reader :contract

      # The original, unmodified input that was validated
      #
      # @return [Hash, nil]
      #
      # @api private
      attr_reader :input

      # Initialize a new result
      #
      # @api private
      def initialize(schema_result, context, options, contract: nil, input: nil)
        @schema_result = schema_result
        @context = context
        @options = options
        @contract = contract
        @input = input
        @errors = initialize_errors
      end

      # Return values wrapper with the input processed by schema
      #
      # @return [Values]
      #
      # @api public
      def values
        @values ||= Values.new(schema_result.to_h)
      end

      # Get error set
      #
      # @!macro errors-options
      #   @param [Hash] new_options
      #   @option new_options [Symbol] :locale Set locale for messages
      #   @option new_options [Boolean] :hints Enable/disable hints
      #   @option new_options [Boolean] :full Get messages that include key names
      #
      # @return [MessageSet]
      #
      # @api public
      def errors(new_options = EMPTY_HASH)
        new_options.empty? ? @errors : @errors.with(schema_errors(new_options), new_options)
      end

      # Check if result is successful
      #
      # @return [Bool]
      #
      # @api public
      def success?
        @errors.empty?
      end

      # Check if result is not successful
      #
      # @return [Bool]
      #
      # @api public
      def failure?
        !success?
      end

      # Check if values include an error for the provided key
      #
      # @api public
      def error?(key)
        errors.any? { |msg| Schema::Path[msg.path].include?(Schema::Path[key]) }
      end

      # All structured fix candidates attached to the errors
      #
      # Every suggestion keeps a reference to the error it came from via
      # Suggestion#error. Identical suggestions reported by multiple errors
      # are returned only once.
      #
      # @return [Array<Suggestion>]
      #
      # @api public
      def suggestions
        @suggestions ||= errors.flat_map { |msg|
          next EMPTY_ARRAY unless msg.respond_to?(:suggestions)

          msg.suggestions.map { |suggestion| suggestion.with_error(msg) }
        }.uniq.freeze
      end

      # Suggestions that can all be applied together
      #
      # This is the maximal conflict-free set: every suggestion in it is
      # compatible with every other one, regardless of application order.
      #
      # @return [Array<Suggestion>]
      #
      # @api public
      def compatible_suggestions
        @compatible_suggestions ||= (suggestions - conflicting_suggestions).freeze
      end

      # Suggestions that cannot be merged automatically
      #
      # These are mutually exclusive with at least one other suggestion -
      # the caller must explicitly choose which ones to apply.
      #
      # @return [Array<Suggestion>]
      #
      # @api public
      def conflicting_suggestions
        @conflicting_suggestions ||= conflict_groups.flatten.freeze
      end

      # Groups of mutually exclusive suggestions
      #
      # Each group contains suggestions that conflict with one another
      # (same path with different replacements, parent/child paths, array
      # index shifts). At most a conflict-free subset of each group can be
      # applied together with the compatible suggestions.
      #
      # @return [Array<Array<Suggestion>>]
      #
      # @api public
      def conflict_groups
        @conflict_groups ||= suggestions.each_with_object([]) { |suggestion, groups|
          overlapping = groups.select { |group|
            group.any? { |other| suggestion.conflicts_with?(other) }
          }

          if overlapping.empty?
            groups << [suggestion]
          else
            merged = overlapping.each_with_object([]) { |group, acc|
              acc.concat(group)
            } << suggestion
            groups.reject! { |group| overlapping.include?(group) }
            groups << merged
          end
        }.select { |group| group.size > 1 }.map(&:freeze).freeze
      end

      # Apply the explicitly selected suggestions to a copy of the original
      # input and re-validate it exactly once
      #
      # This never modifies the original input or this result, and it never
      # loops: the new result is returned as-is, even when it still contains
      # errors (possibly with new suggestions).
      #
      # @param [Array<Suggestion>] selected A conflict-free set of suggestions
      #
      # @raise [ConflictingSuggestionsError] when the selection contains
      #   suggestions that conflict with each other
      # @raise [InvalidSuggestionError] when a selected suggestion does not
      #   come from this result
      #
      # @return [Result] A new result produced from the fixed input
      #
      # @api public
      def apply_suggestions(selected = compatible_suggestions)
        unless contract && input
          raise InvalidSuggestionError,
                "suggestions can only be applied to results built by a contract"
        end

        selected = Array(selected)

        unknown = selected.reject { |suggestion| suggestions.include?(suggestion) }
        unless unknown.empty?
          raise InvalidSuggestionError, "#{unknown.inspect} did not come from this result"
        end

        selected.combination(2) do |one, two|
          if one.conflicts_with?(two)
            raise ConflictingSuggestionsError, "#{one.inspect} conflicts with #{two.inspect}"
          end
        end

        fixed_input = selected.reduce(Suggestion.deep_dup(input)) { |acc, suggestion|
          suggestion.apply(acc)
        }

        contract.call(fixed_input)
      end

      # Check if the base schema (without rules) includes an error for the provided key
      #
      # @api private
      def schema_error?(key)
        schema_result.error?(key)
      end

      # Check if the rules includes an error for the provided key
      #
      # @api private
      def rule_error?(key)
        !schema_error?(key) && error?(key)
      end

      # Check if the result contains any base rule errors
      #
      # @api private
      def base_rule_error?
        !errors.filter(:base?).empty?
      end

      # Check if there's any error for the provided key
      #
      # This does not consider errors from the nested values
      #
      # @api private
      def base_error?(key)
        schema_result.errors.any? { |error|
          key_path = Schema::Path[key]
          err_path = Schema::Path[error.path]

          next unless key_path.same_root?(err_path)

          key_path == err_path
        }
      end

      # Add a new error for the provided key
      #
      # @api private
      def add_error(error)
        @errors.add(error)
        self
      end

      # Read a value under provided key
      #
      # @param [Symbol] key
      #
      # @return [Object]
      #
      # @api public
      def [](key)
        values[key]
      end

      # Check if a key was set
      #
      # @param [Symbol] key
      #
      # @return [Bool]
      #
      # @api public
      def key?(key)
        values.key?(key)
      end

      # Coerce to a hash
      #
      # @api public
      def to_h
        values.to_h
      end

      # Return a string representation
      #
      # @api public
      def inspect
        if context.empty?
          "#<#{self.class}#{to_h} errors=#{errors.to_h}>"
        else
          "#<#{self.class}#{to_h} errors=#{errors.to_h} context=#{context.each.to_h}>"
        end
      end

      # Freeze result and its error set
      #
      # @api private
      def freeze
        values.freeze
        errors.freeze
        suggestions
        compatible_suggestions
        conflicting_suggestions
        conflict_groups
        super
      end

      # Pattern matching
      #
      # @api private
      def deconstruct_keys(keys)
        values.deconstruct_keys(keys)
      end

      # Pattern matching
      #
      # @api private
      def deconstruct
        [values, context.each.to_h]
      end

      private

      # @api private
      def initialize_errors(options = self.options)
        MessageSet.new(schema_errors(options), options)
      end

      # @api private
      def schema_errors(options)
        schema_result.message_set(options).to_a
      end
    end
  end
end
