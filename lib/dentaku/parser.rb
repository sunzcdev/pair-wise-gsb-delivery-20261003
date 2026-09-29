require_relative './ast'
require 'dentaku/source_range'

module Dentaku
  class Parser
    AST_OPERATIONS = {
      add:      AST::Addition,
      subtract: AST::Subtraction,
      multiply: AST::Multiplication,
      divide:   AST::Division,
      pow:      AST::Exponentiation,
      negate:   AST::Negation,
      mod:      AST::Modulo,

      bitor:    AST::BitwiseOr,
      bitand:   AST::BitwiseAnd,
      bitshiftleft:  AST::BitwiseShiftLeft,
      bitshiftright: AST::BitwiseShiftRight,

      lt:       AST::LessThan,
      gt:       AST::GreaterThan,
      le:       AST::LessThanOrEqual,
      ge:       AST::GreaterThanOrEqual,
      ne:       AST::NotEqual,
      eq:       AST::Equal,

      and:      AST::And,
      or:       AST::Or,
      xor:      AST::Xor,
    }.freeze

    attr_reader :input, :output, :operations, :arities, :case_sensitive

    def initialize(tokens, options = {})
      @input             = tokens.dup
      @output            = []
      @operations        = options.fetch(:operations, [])
      @arities           = options.fetch(:arities, [])
      @function_registry = options.fetch(:function_registry, nil)
      @case_sensitive    = options.fetch(:case_sensitive, false)
      @skip_indices      = []
      @operation_tokens  = []
    end

    def consume(count = 2, token: nil)
      operation = operations.pop
      operation_token = @operation_tokens.pop
      fail! :invalid_statement if operation.nil?

      output_size = output.length
      args_size = operation.arity || count
      min_size = operation.arity || operation.min_param_count || count
      max_size = operation.arity || operation.max_param_count || count

      if output_size < min_size || args_size < min_size
        expected = min_size == max_size ? min_size : min_size..max_size
        fail! :too_few_operands, operation: operation, expected: expected, actual: output_size,
              token: operation_token || token
      end

      if (output_size > max_size && operations.empty?) || args_size > max_size
        expected = min_size == max_size ? min_size : min_size..max_size
        fail! :too_many_operands, operation: operation, expected: expected, actual: output_size,
              token: operation_token || token
      end

      args = []
      if operation == AST::Array && output.empty?
        # special case: empty array literal '{}'
        output.push(operation.new)
      else
        fail! :invalid_statement if output_size < args_size
        args = Array.new(args_size) { output.pop }.reverse
        node = operation.new(*args)
        node.source_range ||= node_range(args, operation_token, token)
        output.push node
      end

      if operation.respond_to?(:callback) && !operation.callback.nil?
        operation.callback.call(args)
      end
    rescue ::ArgumentError => e
      range = node_range(args, operation_token, token) ||
              (operation_token && operation_token.source_range) ||
              (token && token.source_range)

      if e.is_a?(Dentaku::ArgumentError)
        Dentaku::Error.attach_source(e, range) if range
        raise
      end

      error = Dentaku::ArgumentError.for(:invalid_value)
      error.attach_source(range) if range
      raise error, e.message
    rescue NodeError => e
      range = operand_range(e.operand, args) || node_range(args, operation_token, token)
      error = ParseError.for(:node_invalid, operation: operation, operand: e.operand, expected: e.expected, actual: e.actual)
      error.attach_source(range) if range
      raise error, e.message
    rescue ParseError => e
      # raised while building the node itself (e.g. MAP() rejecting a
      # non-identifier argument) -- point at the construct being parsed
      range = node_range(args, operation_token, token) ||
              (operation_token && operation_token.source_range) ||
              (token && token.source_range)
      e.attach_source(range) if range
      raise
    end

    def parse
      return AST::Nil.new if input.empty?

      i = 0
      while i < input.length
        if @skip_indices.include?(i)
          i += 1
          next
        end

        token = input[i]
        lookahead = input[i + 1]
        process_token(token, lookahead, i)
        i += 1
      end

      consume while operations.any?

      fail! :invalid_statement unless output.count == 1

      output.first
    end

    def operation(token)
      AST_OPERATIONS.fetch(token.value)
    end

    def function(token)
      function_registry.get(token.value)
    end

    def function_registry
      @function_registry ||= Dentaku::AST::FunctionRegistry.new
    end

    private

    def process_token(token, lookahead, index)
      case token.category
      when :datetime      then output << AST::DateTime.new(token)
      when :numeric       then output << AST::Numeric.new(token)
      when :logical       then output << AST::Logical.new(token)
      when :string        then output << AST::String.new(token)
      when :identifier    then output << AST::Identifier.new(token, case_sensitive: case_sensitive)
      when :operator, :comparator, :combinator
        handle_operator(token, lookahead)
      when :null
        output << AST::Nil.new.tap { |node| node.source_range = token.source_range }
      when :function
        handle_function(token)
      when :case
        handle_case(token)
      when :access
        handle_access(token)
      when :array
        handle_array(token)
      when :grouping
        handle_grouping(token, lookahead, index)
      else
        fail! :not_implemented_token_category, token_category: token.category
      end
    end

    def handle_operator(token, lookahead)
      op_class = operation(token).resolve_class(lookahead)
      if op_class.right_associative?
        consume while operations.last && operations.last < AST::Operation && op_class.precedence < operations.last.precedence
      else
        consume while operations.last && operations.last < AST::Operation && op_class.precedence <= operations.last.precedence
      end
      push_operation op_class, token
    end

    def handle_function(token)
      func = function(token)
      fail! :undefined_function, function_name: token.value, token: token if func.nil?
      arities.push 0
      push_operation func, token
    end

    def handle_case(token)
      # We always operate on the innermost (most recent) CASE on the stack.
      case_index = operations.rindex(AST::Case) || -1
      token_index = case_index + 1

      case token.value
      when :open
        # Start a new CASE context.
        push_operation AST::Case, token
        arities.push(0)

      when :close
        # Finalize any trailing THEN/ELSE expression still on the stack.
        if operations[token_index] == AST::CaseThen
          consume_until(AST::Case)
          push_operation(AST::CaseConditional, token)
          consume(2)
          arities[-1] += 1
        elsif operations[token_index] == AST::CaseElse
          consume_until(AST::Case)
          arities[-1] += 1
        end
        fail! :unprocessed_token, token_name: token.value unless operations.last == AST::Case
        consume(arities.pop.succ, token: token)

      when :when
        if operations[token_index] == AST::CaseThen
          # Close out previous WHEN/THEN pair.
          consume_until([AST::CaseWhen, AST::Case])
          push_operation(AST::CaseConditional, token)
          consume(2)
          arities[-1] += 1
        elsif operations[token_index].nil? ||
              (operations[token_index] != AST::CaseWhen && operations[token_index] < AST::Operation)
          # First WHEN: fold any operators pending from an unparenthesized
          # switch expression, then finalize the switch variable.
          consume_until(AST::Case)
          push_operation(AST::CaseSwitchVariable, token)
          consume
        end
        push_operation(AST::CaseWhen, token)

      when :then
        if operations[token_index] == AST::CaseWhen
          consume_until([AST::CaseThen, AST::Case])
        end
        push_operation(AST::CaseThen, token)

      when :else
        if operations[token_index] == AST::CaseThen
          consume_until(AST::Case)
          push_operation(AST::CaseConditional, token)
          consume(2)
          arities[-1] += 1
        end
        push_operation(AST::CaseElse, token)

      else
        fail! :unknown_case_token, token_name: token.value
      end
    end

    def consume_until(target)
      matcher =
        case target
        when Array then ->(op) { target.include?(op) }
        else ->(op) { op == target }
        end

      consume while operations.any? && !matcher.call(operations.last)
    end

    def handle_access(token)
      case token.value
      when :lbracket
        push_operation AST::Access, token

      when :rbracket
        consume while operations.any? && operations.last != AST::Access
        fail! :unbalanced_bracket, token: token unless operations.last == AST::Access
        consume(2, token: token)
      end
    end

    def handle_array(token)
      case token.value
      when :array_start
        push_operation AST::Array, token
        arities.push 0

      when :array_end
        consume while operations.any? && operations.last != AST::Array
        fail! :unbalanced_bracket, token: token unless operations.last == AST::Array
        consume(arities.pop.succ, token: token)
      end
    end

    def handle_grouping(token, lookahead, token_index)
      case token.value
      when :open
        if lookahead && lookahead.value == :close
          # empty grouping (e.g. function with zero arguments) — we trigger consume later
          # skip to the end
          @skip_indices << token_index + 1
          arities.pop
          consume(0, token: lookahead)
        else
          push_operation AST::Grouping, token
        end

      when :close
        consume while operations.any? && operations.last != AST::Grouping
        lparen = operations.pop
        @operation_tokens.pop
        fail! :unbalanced_parenthesis, token: token unless lparen == AST::Grouping
        if operations.last && operations.last < AST::Function
          consume(arities.pop.succ, token: token)
        end

      when :comma
        fail! :invalid_statement if arities.empty?
        arities[-1] += 1
        consume while operations.any? && operations.last != AST::Grouping && operations.last != AST::Array

      else
        fail! :unknown_grouping_token, token_name: token.value
      end
    end

    # keeps a token alongside every operation so the node built by
    # `consume` can be mapped back to the original expression
    def push_operation(operation, token)
      operations.push operation
      @operation_tokens.push token
    end

    # smallest range covering the operands, the operator/function token,
    # and the token that closed the construct (e.g. the closing paren of
    # a function call), so a node highlights its full source text
    def node_range(args, operation_token, trigger_token)
      ranges = Array(args).compact.map(&:source_range).compact
      ranges << operation_token.source_range if operation_token && operation_token.source_range
      ranges << trigger_token.source_range if trigger_token && trigger_token.source_range
      return nil if ranges.empty?

      SourceRange.new(
        ranges.map(&:start_offset).min,
        ranges.map(&:end_offset).max
      )
    end

    def operand_range(operand, args)
      return nil if args.nil? || args.empty?

      node =
        case operand
        when :left  then args.first
        when :right then args[1] || args.last
        end

      node && node.source_range
    end

    def fail!(reason, **meta)
      token = meta.delete(:token)
      error = ParseError.for(reason, **meta)
      error.attach_source(token.source_range) if token && token.source_range
      raise error
    end
  end
end
