require_relative './ast'

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
      @function_tokens   = []
      @grouping_tokens   = []
      @last_token        = nil
    end

    def consume(count = 2)
      operation = operations.pop
      fail! :invalid_statement if operation.nil?

      output_size = output.length
      args_size = operation.arity || count
      min_size = operation.arity || operation.min_param_count || count
      max_size = operation.arity || operation.max_param_count || count

      if output_size < min_size || args_size < min_size
        expected = min_size == max_size ? min_size : min_size..max_size
        fail! :too_few_operands, operation: operation, expected: expected, actual: output_size
      end

      if (output_size > max_size && operations.empty?) || args_size > max_size
        expected = min_size == max_size ? min_size : min_size..max_size
        fail! :too_many_operands, operation: operation, expected: expected, actual: output_size
      end

      args = []
      if operation == AST::Array && output.empty?
        # special case: empty array literal '{}'
        output.push(operation.new)
      else
        fail! :invalid_statement if output_size < args_size
        args = Array.new(args_size) { output.pop }.reverse
        node = operation.new(*args)
        node.location = node_location(operation, args)
        output.push node
      end

      if operation.respond_to?(:callback) && !operation.callback.nil?
        operation.callback.call(args)
      end
    rescue ::ArgumentError => e
      raise if e.is_a?(Dentaku::ArgumentError)
      error = Dentaku::ArgumentError.for(:invalid_value)
      error.location ||= span_location(args)
      raise error, e.message
    rescue ParseError => e
      e.location ||= span_location(args)
      raise
    rescue NodeError => e
      offending = e.operand == :left ? args.first : args.last
      error = ParseError.for(:node_invalid, operation: operation, operand: e.operand, expected: e.expected, actual: e.actual)
      error.location ||= offending && offending.location
      raise error, e.message
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
        @last_token = token
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
        output << AST::Nil.new
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
      operations.push op_class
    end

    def handle_function(token)
      func = function(token)
      fail! :undefined_function, function_name: token.value, token: token if func.nil?
      arities.push 0
      operations.push func
      @function_tokens.push(token)
    end

    def handle_case(token)
      # We always operate on the innermost (most recent) CASE on the stack.
      case_index = operations.rindex(AST::Case) || -1
      token_index = case_index + 1

      case token.value
      when :open
        # Start a new CASE context.
        operations.push AST::Case
        arities.push(0)

      when :close
        # Finalize any trailing THEN/ELSE expression still on the stack.
        if operations[token_index] == AST::CaseThen
          consume_until(AST::Case)
          operations.push(AST::CaseConditional)
          consume(2)
          arities[-1] += 1
        elsif operations[token_index] == AST::CaseElse
          consume_until(AST::Case)
          arities[-1] += 1
        end
        fail! :unprocessed_token, token_name: token.value unless operations.last == AST::Case
        consume(arities.pop.succ)

      when :when
        if operations[token_index] == AST::CaseThen
          # Close out previous WHEN/THEN pair.
          consume_until([AST::CaseWhen, AST::Case])
          operations.push(AST::CaseConditional)
          consume(2)
          arities[-1] += 1
        elsif operations[token_index].nil? ||
              (operations[token_index] != AST::CaseWhen && operations[token_index] < AST::Operation)
          # First WHEN: fold any operators pending from an unparenthesized
          # switch expression, then finalize the switch variable.
          consume_until(AST::Case)
          operations.push(AST::CaseSwitchVariable)
          consume
        end
        operations.push(AST::CaseWhen)

      when :then
        if operations[token_index] == AST::CaseWhen
          consume_until([AST::CaseThen, AST::Case])
        end
        operations.push(AST::CaseThen)

      when :else
        if operations[token_index] == AST::CaseThen
          consume_until(AST::Case)
          operations.push(AST::CaseConditional)
          consume(2)
          arities[-1] += 1
        end
        operations.push(AST::CaseElse)

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
        operations.push AST::Access

      when :rbracket
        consume while operations.any? && operations.last != AST::Access
        fail! :unbalanced_bracket, token: token unless operations.last == AST::Access
        consume
      end
    end

    def handle_array(token)
      case token.value
      when :array_start
        operations.push AST::Array
        arities.push 0

      when :array_end
        consume while operations.any? && operations.last != AST::Array
        fail! :unbalanced_bracket, token: token unless operations.last == AST::Array
        consume(arities.pop.succ)
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
          consume(0)
          extend_location(output.last, lookahead)
        else
          operations.push AST::Grouping
          @grouping_tokens.push(token)
        end

      when :close
        consume while operations.any? && operations.last != AST::Grouping
        lparen = operations.pop
        fail! :unbalanced_parenthesis, token: token unless lparen == AST::Grouping
        open_token = @grouping_tokens.pop
        if operations.last && operations.last < AST::Function
          consume(arities.pop.succ)
          extend_location(output.last, token)
        else
          extend_location(output.last, token, open_token)
        end

      when :comma
        fail! :invalid_statement if arities.empty?
        arities[-1] += 1
        consume while operations.any? && operations.last != AST::Grouping && operations.last != AST::Array

      else
        fail! :unknown_grouping_token, token_name: token.value
      end
    end

    # source range for a freshly built node: functions span from their name
    # token, everything else spans its operands
    def node_location(operation, args)
      if operation.is_a?(Class) && operation <= AST::Function
        token = @function_tokens.pop
        start = token && token.location && token.location.begin
        finish = args.last && args.last.location && args.last.location.end
        start && finish ? start...finish : (token && token.location)
      else
        span_location(args)
      end
    end

    def span_location(args)
      first = args.first && args.first.location
      last = args.last && args.last.location
      first && last ? first.begin...last.end : first
    end

    def extend_location(node, token, start_token = nil)
      return unless node && node.location && token && token.location

      start = start_token && start_token.location ? start_token.location.begin : node.location.begin
      node.location = start...token.location.end
    end

    def fail!(reason, **meta)
      error = ParseError.for(reason, **meta)
      error.location ||= meta[:token] && meta[:token].location
      error.location ||= @last_token && @last_token.location
      raise error
    end
  end
end
