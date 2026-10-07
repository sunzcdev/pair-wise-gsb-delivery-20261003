# frozen_string_literal: true
require 'mustermann/error'
require 'uri'

module Mustermann
  # Incremental, streaming matcher for Mustermann patterns.
  #
  # A backend endpoint that receives a long path in several chunks can use an
  # incremental session to decide, as each chunk arrives, whether the data
  # received so far can still lead to a match - without rescanning the input
  # received so far. Every chunk is processed exactly once, so the total work
  # is proportional to the input length, not to the number of chunks.
  #
  # @example
  #   require 'mustermann'
  #
  #   session = Mustermann::Incremental.new(Mustermann.new('/:name(/*rest)?'))
  #   session << '/foo'    # => :continue
  #   session << '/bar'    # => :continue
  #   result = session.finish
  #   result.status        # => :match
  #   result.params        # => { 'name' => 'foo', 'rest' => 'bar' }
  #
  # Sessions support patterns made up of literals, captures, optional segments
  # and splats. Patterns using other features (unions, look-aheads, URI
  # template expressions, the :capture or :except options, non-AST pattern
  # types, ...) are rejected with {UnsupportedPatternError} when the session
  # is created.
  #
  # Percent escapes and multi-byte UTF-8 characters may be split across chunk
  # boundaries. Truncated input that can only be detected once no more data
  # will arrive is reported by {Session#finish} raising {IncompleteInputError}.
  module Incremental
    # Raised when a pattern cannot be compiled into an incremental program.
    UnsupportedPatternError = Class.new(Mustermann::Error)

    # Raised when the input can only be identified as incomplete once no more
    # input will arrive (a truncated UTF-8 character or percent escape).
    IncompleteInputError = Class.new(Mustermann::Error)

    # Raised when the input contains bytes that are not valid UTF-8.
    InvalidInputError = Class.new(Mustermann::Error)

    # Raised when feeding a session that has already been finished.
    SessionClosedError = Class.new(Mustermann::Error)

    # Characters excluded by the default capture character class [^/\?#].
    # @!visibility private
    CAPTURE_EXCLUDED = '/?#'.freeze

    # Creates a new incremental matching session for the given pattern.
    #
    # @param [Mustermann::Pattern] pattern the pattern to match against
    # @return [Mustermann::Incremental::Session] new session
    # @raise [Mustermann::Incremental::UnsupportedPatternError] if the pattern cannot be matched incrementally
    def self.new(pattern)
      Session.new(pattern)
    end

    # Immutable, compiled form of a pattern: a flat list of instructions
    # executed by the {Session} virtual machine. Instructions:
    #
    #   [:lit, char]             consume the given character
    #   [:class, chars, negated] consume one character (not) contained in chars
    #   [:split, first, second]  fork, first has higher priority than second
    #   [:jmp, target]           jump to target
    #   [:save, capture, slot]   record current position for capture (0: start, 1: stop)
    #   [:match]                 accept
    #
    # Programs are immutable and contain no per-match state, so sessions never
    # share mutable captures with each other.
    #
    # @!visibility private
    class Program
      # @return [Array<Array>] instruction list
      attr_reader :instructions

      # @return [Array<String>] capture names, indexed by capture id
      attr_reader :captures

      # @return [Integer] instruction pointer accepting a match
      attr_reader :match_pc

      def initialize(instructions, captures)
        @instructions = instructions.map!(&:freeze).freeze
        @captures     = captures.freeze
        @match_pc     = instructions.size - 1
        @indexes      = {}
        captures.each_with_index { |name, index| (@indexes[name] ||= []) << index }
        @indexes.freeze
      end

      # @return [Hash{String => Array<Integer>}] capture ids grouped by name, in definition order
      def indexes
        @indexes
      end
    end

    # Compiles a supported AST pattern into a {Program}.
    # @!visibility private
    class Compiler
      URI_PARSER = defined?(URI::RFC2396_PARSER) ? URI::RFC2396_PARSER : URI::RFC2396_Parser.new

      def self.compile(pattern)
        new(pattern).compile
      end

      def initialize(pattern)
        unless defined?(Mustermann::AST::Pattern) && pattern.is_a?(Mustermann::AST::Pattern)
          raise UnsupportedPatternError, "incremental matching is only supported for AST based patterns, got #{pattern.class}"
        end

        options = pattern.options
        %i[capture except].each do |option|
          next unless options[option]
          raise UnsupportedPatternError, "incremental matching does not support the #{option.inspect} option"
        end

        @pattern            = pattern
        @uri_decode         = pattern.uri_decode
        @space_matches_plus = options.fetch(:space_matches_plus, true)
        @greedy             = options.fetch(:greedy, true)
        @instructions       = []
        @captures           = []
        @representations    = {}
      end

      def compile
        compile_node(@pattern.to_ast)
        emit([:match])
        Program.new(@instructions, @captures)
      end

      private

      def emit(instruction)
        @instructions << instruction
        @instructions.size - 1
      end

      def compile_node(node)
        case node.type
        when :root, :group        then node.payload.each { |child| compile_node(child) }
        when :separator           then node.payload.each_char { |char| emit([:lit, char]) }
        when :char                then emit_alternatives(representations(node.payload))
        when :optional            then compile_optional(node.payload)
        when :splat, :named_splat then compile_splat(node)
        when :capture             then compile_capture(node)
        else raise UnsupportedPatternError, "incremental matching does not support #{node.type} nodes in #{@pattern.to_s.inspect}"
        end
      end

      def compile_capture(node)
        unsupported(node, 'constrained captures') if node.constraint || node.qualifier
        index = register_capture(node.name)
        emit([:save, index, 0])
        loop_start = @instructions.size
        emit([:class, CAPTURE_EXCLUDED, true])
        exit_pos = @instructions.size + 1
        emit(@greedy ? [:split, loop_start, exit_pos] : [:split, exit_pos, loop_start])
        emit([:save, index, 1])
      end

      def compile_splat(node)
        unsupported(node, 'constrained splats') if node.constraint
        index = register_capture(node.name)
        emit([:save, index, 0])
        loop_start = @instructions.size
        # splats always compile to .*? (non-greedy, . does not match a newline)
        emit([:split, loop_start + 3, loop_start + 1])
        emit([:class, "\n", true])
        emit([:jmp, loop_start])
        emit([:save, index, 1])
      end

      def compile_optional(payload)
        split_pos = @instructions.size
        emit([:split, split_pos + 1, nil])
        compile_node(payload)
        @instructions[split_pos][2] = @instructions.size
      end

      # Emits instructions matching exactly one of the given strings, trying
      # them in order (mirrors the regex alternation order).
      def emit_alternatives(strings)
        first, *rest = strings
        if rest.empty?
          first.each_char { |char| emit([:lit, char]) }
        else
          split_pos = @instructions.size
          emit([:split, split_pos + 1, nil])
          first.each_char { |char| emit([:lit, char]) }
          jmp_pos = emit([:jmp, nil])
          @instructions[split_pos][2] = @instructions.size
          emit_alternatives(rest)
          @instructions[jmp_pos][1] = @instructions.size
        end
      end

      # All raw representations of a literal character, mirroring
      # Mustermann::AST::Compiler.char_representations.
      def representations(char)
        @representations[char] ||= compute_representations(char).freeze
      end

      def compute_representations(char)
        return [char] unless @uri_decode
        representations  = char_representations(char)
        representations += char_representations('+') if char == ' ' && @space_matches_plus
        representations.uniq
      end

      def char_representations(char)
        escaped = URI_PARSER.escape(char, /./)
        [char, escaped.upcase, escaped.downcase].uniq
      end

      def register_capture(name)
        @captures << name
        @captures.size - 1
      end

      def unsupported(node, feature)
        raise UnsupportedPatternError, "incremental matching does not support #{feature} in #{@pattern.to_s.inspect}"
      end
    end

    # The final outcome of a {Session}, returned by {Session#finish}.
    class Result
      # @return [Symbol] :match or :mismatch
      attr_reader :status

      # @return [Hash, nil] Sinatra style params if status is :match, nil otherwise
      attr_reader :params

      # @!visibility private
      def initialize(status, params)
        @status = status
        @params = params
        freeze
      end

      # @return [Boolean] whether the input matched the pattern
      def matched?
        @status == :match
      end

      # @!visibility private
      def inspect
        "#<#{self.class.name}: #{status}#{" #{params.inspect}" if params}>"
      end
    end

    # A single incremental matching session.
    #
    # Feed chunks with {#<<} (aliased as {#feed}), which returns the session
    # status:
    #
    # * +:continue+ - the input received so far can still lead to a match
    # * +:mismatch+ - no input starting with the received data can match
    #
    # Once all input has been fed, call {#finish}, which returns a {Result}
    # exposing the third state (+:match+ or +:mismatch+) and the params.
    #
    # Every session keeps its own captures; sessions (even for the same
    # pattern) never share mutable state and can be advanced independently.
    # Feeding the same path byte by byte or in one go yields the same params.
    class Session
      # @!visibility private
      EMPTY_SAVES = {}.freeze

      # @return [Mustermann::Pattern] the pattern being matched
      attr_reader :pattern

      # @return [Symbol] current status: :continue, :mismatch or (after #finish) :match
      attr_reader :status

      # @return [Integer] number of input characters consumed so far
      attr_reader :input_length

      # @param [Mustermann::Pattern] pattern the pattern to match against
      # @raise [Mustermann::Incremental::UnsupportedPatternError] if the pattern cannot be matched incrementally
      def initialize(pattern)
        @pattern      = pattern
        @program      = Compiler.compile(pattern)
        @input        = String.new(encoding: Encoding::UTF_8)
        @pending      = String.new # raw bytes not yet forming a complete character
        @marks        = Array.new(@program.instructions.size)
        @generation   = 0
        @input_length = 0
        @clist        = []
        @status       = :continue
        @result       = nil
        @finished     = false
        @generation  += 1
        closure(@clist, 0, EMPTY_SAVES)
      end

      # Feeds a chunk of input to the session. Percent escapes and multi-byte
      # UTF-8 characters may be split across chunks. Once the session reached
      # +:mismatch+, further input is ignored.
      #
      # @param [String] chunk next chunk of the input
      # @return [Symbol] :continue if the input can still lead to a match, :mismatch otherwise
      # @raise [Mustermann::Incremental::InvalidInputError] if the chunk contains invalid UTF-8
      # @raise [Mustermann::Incremental::SessionClosedError] if the session has been finished
      def <<(chunk)
        raise SessionClosedError, 'cannot feed a session that has been finished' if @finished
        return @status if @status == :mismatch
        @pending << chunk.to_str.b
        drain
        @status
      rescue InvalidInputError
        @clist.clear
        @status = :mismatch
        raise
      end
      alias_method :feed, :<<

      # @return [Boolean] whether the session has been finished
      def finished?
        @finished
      end

      # Finishes the session: no more input will arrive. Safe to call more
      # than once.
      #
      # @return [Mustermann::Incremental::Result] the final result
      # @raise [Mustermann::Incremental::IncompleteInputError] if the input ends
      #   with an incomplete UTF-8 character or percent escape
      def finish
        return @result if @result
        @finished = true
        unless @pending.empty?
          raise IncompleteInputError, "input ends with an incomplete UTF-8 character (#{hex(@pending)})"
        end
        @result = build_result
        @status = @result.status
        @result
      end

      # @return [Hash, nil] captured params if the session finished with a match
      def params
        @result&.params
      end

      # @!visibility private
      def inspect
        "#<#{self.class.name}: #{@pattern.to_s.inspect} #{@status}>"
      end

      private

      # Processes as many complete characters as the pending bytes hold,
      # keeping an incomplete trailing character for the next chunk.
      def drain
        return if @pending.empty?
        string = @pending.dup.force_encoding(Encoding::UTF_8)
        valid, @pending = split_valid(string)
        return if valid.empty?
        @input << valid
        valid.each_char { |char| step(char) }
        @status = :mismatch if @clist.empty?
      end

      # Splits a string into the longest valid UTF-8 prefix and a trailing
      # incomplete character (kept as pending bytes). Raises InvalidInputError
      # for bytes that can never form a valid character.
      def split_valid(string)
        return [string, String.new] if string.valid_encoding?
        1.upto(3) do |length|
          next if string.bytesize < length
          head = string.byteslice(0, string.bytesize - length)
          next unless head.valid_encoding?
          tail = string.byteslice(string.bytesize - length, length)
          return [head, tail.b] if incomplete_utf8?(tail)
          raise InvalidInputError, "invalid byte sequence in UTF-8 input (#{hex(tail)})"
        end
        raise InvalidInputError, "invalid byte sequence in UTF-8 input (#{hex(string)})"
      end

      # Whether the given bytes are a proper prefix of a valid UTF-8 character.
      def incomplete_utf8?(binary)
        bytes = binary.bytes
        size  = case bytes[0]
                when 0xC2..0xDF then 2
                when 0xE0..0xEF then 3
                when 0xF0..0xF4 then 4
                else return false
                end
        bytes.size < size && bytes[1..].all? { |byte| byte.between?(0x80, 0xBF) }
      end

      # Advances all live states by one character. Each character is processed
      # exactly once, no matter how many chunks the input arrived in.
      def step(char)
        @input_length += 1
        @generation   += 1
        instructions = @program.instructions
        nlist = []
        @clist.each do |pc, saves|
          instruction = instructions[pc]
          case instruction[0]
          when :lit
            closure(nlist, pc + 1, saves) if instruction[1] == char
          when :class
            member = instruction[1].include?(char)
            closure(nlist, pc + 1, saves) if instruction[2] ? !member : member
          end
        end
        @clist = nlist
      end

      # Computes the epsilon closure of a state, in priority order, recording
      # capture positions. States are deduplicated by instruction pointer per
      # generation; the first (highest priority) thread to reach an
      # instruction wins, mirroring regex backtracking order.
      def closure(list, pc, saves)
        instructions = @program.instructions
        stack = [[pc, saves]]
        until stack.empty?
          pc, saves = stack.pop
          next if @marks[pc] == @generation
          @marks[pc] = @generation
          instruction = instructions[pc]
          case instruction[0]
          when :split
            stack.push([instruction[2], saves], [instruction[1], saves])
          when :jmp
            stack.push([instruction[1], saves])
          when :save
            updated = saves.dup
            pair    = updated[instruction[1]]
            pair    = pair ? pair.dup : [nil, nil]
            pair[instruction[2]]  = @input_length
            updated[instruction[1]] = pair
            stack.push([pc + 1, updated])
          else
            list.push([pc, saves])
          end
        end
        list
      end

      def build_result
        state = @clist.find { |pc, _| pc == @program.match_pc }
        return Result.new(:match, extract_params(state[1])) if state
        if @clist.any? && @input.match?(/%[0-9A-Fa-f]?\z/)
          raise IncompleteInputError, 'input ends with an incomplete percent escape'
        end
        Result.new(:mismatch, nil)
      end

      def extract_params(saves)
        @program.indexes.each_with_object({}) do |(name, ids), params|
          values = ids.map do |id|
            pair = saves[id]
            pair ? @input[pair[0]...pair[1]] : nil
          end
          params[name] = if ids.size < 2 && !always_array?(name)
            map_param(name, values.first)
          else
            values.flat_map { |value| map_param(name, value) }
          end
        end
      end

      def map_param(name, value)
        @pattern.send(:map_param, name, value)
      end

      def always_array?(name)
        name == 'splat' || name == 'captures'
      end

      def hex(string)
        string.bytes.map { |byte| format('%%%02X', byte) }.join(' ')
      end
    end

    private_constant :Program, :Compiler
  end
end
