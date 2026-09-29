require 'dentaku/source_range'

module Dentaku
  # Location API shared by every error Dentaku raises. It can also be
  # mixed into foreign exceptions (e.g. a NoMethodError escaping from a
  # custom function body) so that any error surfacing from an evaluation
  # points back at the user's original expression.
  module ErrorLocation
    # the innermost Dentaku::SourceRange this error was observed at --
    # the range an editor should highlight
    def source_range
      @source_range
    end

    # source ranges of enclosing function calls, innermost first
    def call_trace
      @call_trace ||= []
    end

    # Records where the error surfaced. Accepts an AST node (its
    # source_range is used, and function calls are pushed onto the
    # call_trace) or a bare Dentaku::SourceRange. Returns self so raise
    # sites can chain: `raise error.attach_source(range)`.
    def attach_source(node_or_range)
      range =
        if node_or_range.is_a?(Dentaku::SourceRange)
          node_or_range
        elsif node_or_range.respond_to?(:source_range)
          node_or_range.source_range
        end

      return self if range.nil?

      @source_range ||= range

      if !node_or_range.is_a?(Dentaku::SourceRange) &&
         node_or_range.respond_to?(:call_site?) && node_or_range.call_site?
        call_trace.push(range) unless call_trace.last == range
      end

      self
    end
  end

  # Shared by all Dentaku exceptions so that `rescue Dentaku::Error` catches
  # everything the gem raises, including exceptions that subclass Ruby
  # built-ins (Dentaku::ArgumentError, Dentaku::ZeroDivisionError).
  module Error
    include ErrorLocation

    attr_accessor :assigned_to

    # Attaches source location to any exception, including ones raised by
    # user code inside custom functions. The exception's class and message
    # are left untouched -- in particular, no argument values are added.
    def self.attach_source(exception, node_or_range)
      exception.extend(ErrorLocation) unless exception.respond_to?(:attach_source)
      exception.attach_source(node_or_range)
    rescue FrozenError, TypeError
      exception
    end
  end

  class BaseError < StandardError
    include Error
  end

  class UnboundVariableError < BaseError
    attr_reader :unbound_variables

    def initialize(unbound_variables)
      @unbound_variables = unbound_variables
    end
  end

  class MathDomainError < BaseError
    attr_reader :function_name, :arg_count

    def initialize(function_name, args)
      @function_name = function_name
      # only the count is retained: argument values may be sensitive and
      # must not travel with the error result
      @arg_count = args.respond_to?(:length) ? args.length : args
    end
  end

  class NodeError < BaseError
    attr_reader :operand, :expected, :actual

    def initialize(expected, actual, operand)
      @expected = Array(expected)
      @actual = actual
      @operand = operand
    end
  end

  class ParseError < BaseError
    attr_reader :reason, :meta

    def initialize(reason, **meta)
      @reason = reason
      @meta = meta
      super(default_message)
    end

    private_class_method :new

    VALID_REASONS = %i[
      node_invalid too_few_operands too_many_operands undefined_function
      unprocessed_token unknown_case_token unbalanced_bracket
      unbalanced_parenthesis unknown_grouping_token not_implemented_token_category
      invalid_statement
    ].freeze

    def self.for(reason, **meta)
      unless VALID_REASONS.include?(reason)
        raise ::ArgumentError, "Unhandled #{reason}"
      end

      new(reason, **meta)
    end

    private

    def default_message
      case reason
      when :node_invalid
        if meta.key?(:operation)
          expected = Array(meta[:expected]).map { |e| e == :incompatible ? :compatible : e }
          "#{meta[:operation]} requires #{expected.join(', ')} operands, but got #{meta[:actual]}"
        else
          "Node is invalid"
        end
      when :too_few_operands
        "#{meta[:operation]} has too few operands (given #{meta[:actual]}, expected #{meta[:expected]})"
      when :too_many_operands
        "#{meta[:operation]} has too many operands (given #{meta[:actual]}, expected #{meta[:expected]})"
      when :undefined_function
        "Undefined function #{meta[:function_name]}"
      when :unprocessed_token
        "Unprocessed token #{meta[:token_name]}"
      when :unknown_case_token
        "Unknown case token #{meta[:token_name]}"
      when :unbalanced_bracket
        "Unbalanced bracket"
      when :unbalanced_parenthesis
        "Unbalanced parenthesis"
      when :unknown_grouping_token
        "Unknown grouping token #{meta[:token_name]}"
      when :not_implemented_token_category
        "Not implemented for tokens of category #{meta[:token_category]}"
      when :invalid_statement
        "Invalid statement"
      end
    end
  end

  class TokenizerError < BaseError
    attr_reader :reason, :meta

    def initialize(reason, **meta)
      @reason = reason
      @meta = meta
      super(default_message)
    end

    private_class_method :new

    VALID_REASONS = %i[
      parse_error
      too_many_closing_parentheses
      too_many_opening_parentheses
      unexpected_zero_width_match
    ].freeze

    def self.for(reason, **meta)
      unless VALID_REASONS.include?(reason)
        raise ::ArgumentError, "Unhandled #{reason}"
      end

      new(reason, **meta)
    end

    private

    def default_message
      case reason
      when :parse_error
        "parse error at: '#{meta[:at]}'"
      when :too_many_opening_parentheses
        "too many opening parentheses"
      when :too_many_closing_parentheses
        "too many closing parentheses"
      when :unexpected_zero_width_match
        "unexpected zero-width match (:#{meta[:token_category]}) at '#{meta[:at]}'"
      end
    end
  end

  class ArgumentError < ::ArgumentError
    include Error

    attr_reader :reason, :meta

    def initialize(reason, **meta)
      @reason = reason
      @meta = meta
      super(default_message)
    end

    private_class_method :new

    VALID_REASONS = %i[
      incompatible_type
      invalid_operator
      invalid_value
      too_few_arguments
      wrong_number_of_arguments
    ].freeze

    def self.for(reason, **meta)
      unless VALID_REASONS.include?(reason)
        raise ::ArgumentError, "Unhandled #{reason}"
      end

      new(reason, **meta)
    end

    private

    def default_message
      case reason
      when :incompatible_type
        if meta.key?(:function_name)
          "#{meta[:function_name]}() requires #{meta[:expected]} arguments, but got #{meta[:actual].class}"
        else
          "#{meta[:actual].class} is not compatible with #{meta[:expected]}"
        end
      when :invalid_operator
        "#{meta[:operation]} requires operands that respond to #{meta[:operator]}"
      when :invalid_value
        if meta.key?(:expected)
          "'#{meta[:actual]}' is not a valid #{meta[:expected]}"
        elsif meta.key?(:actual)
          "'#{meta[:actual]}' is not a valid value"
        else
          "Invalid value"
        end
      when :too_few_arguments
        "#{meta[:function_name]}() has too few arguments (given #{meta[:actual]}, expected #{expected_count})"
      when :wrong_number_of_arguments
        "#{meta[:function_name]}() has the wrong number of arguments (given #{meta[:actual]}, expected #{expected_count})"
      end
    end

    def expected_count
      expected = meta[:expected]
      expected.is_a?(Range) && expected.end.nil? ? "at least #{expected.begin}" : expected
    end
  end

  class ZeroDivisionError < ::ZeroDivisionError
    include Error
  end
end
