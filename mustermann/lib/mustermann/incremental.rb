# frozen_string_literal: true
require 'mustermann/pattern'
require 'mustermann/error'

module Mustermann
  # Incremental (streaming) matching sessions for Mustermann patterns.
  #
  # A session receives a candidate path in arbitrary chunks and, after every
  # chunk, reports one of three outcomes:
  #
  # * +:continue+   - the input received so far can still be extended to a
  #   full match, keep feeding.
  # * +:mismatch+   - no matter what comes next, the input can never match.
  # * after {Session#finish}: +:match+ (with params) or +:incomplete+ (the
  #   input ended in the middle of the pattern, a percent escape or a UTF-8
  #   character).
  #
  # Matching is byte oriented: percent escapes (+%41+) and multi-byte UTF-8
  # characters may be split across chunks arbitrarily.  Feeding a path byte
  # by byte yields the same params as feeding it in a single chunk, and the
  # work per chunk is proportional to the chunk size - never to the total
  # input size (the already consumed prefix is never rescanned).
  #
  # @example
  #   session = Mustermann::Incremental.new('/:user/files/*path')
  #   session.feed('/ann')      # => #<Mustermann::Incremental::Result :continue>
  #   session.feed('a/files/x') # => #<Mustermann::Incremental::Result :continue>
  #   session.finish            # => #<Mustermann::Incremental::Result :match>
  #   session.params            # => {"user"=>"anna", "path"=>"x"}
  #
  # Only a subset of patterns can be matched incrementally: literal
  # characters, separators, named captures, splats and optional segments.
  # Anything else (unions, URI template expressions, look-aheads, capture
  # constraints, the +:except+ or +:capture+ options, non-AST pattern types)
  # is rejected with {UnsupportedPatternError} when the session is created.
  module Incremental
    # Raised when a pattern cannot be matched incrementally.
    UnsupportedPatternError = Class.new(Mustermann::Error)

    # Creates a new incremental matching session.
    #
    # @param [Mustermann::Pattern, String] input pattern or pattern string
    # @param [Hash] options pattern options (only used for string input)
    # @return [Mustermann::Incremental::Session]
    # @raise [Mustermann::Incremental::UnsupportedPatternError] if the pattern cannot be matched incrementally
    def self.new(input, **options)
      pattern = if input.is_a?(Mustermann::Pattern)
                  input
                else
                  require 'mustermann' unless Mustermann.respond_to?(:new)
                  Mustermann.new(input, **options)
                end
      Session.new(pattern)
    end

    # Set of bytes, used for transitions in the automaton.
    # @!visibility private
    class ByteSet
      FULL = (1 << 256) - 1

      def self.[](*bytes)
        new(bytes.inject(0) { |mask, byte| mask | (1 << byte) })
      end

      def self.except(*bytes)
        new(self[*bytes].mask ^ FULL)
      end

      attr_reader :mask

      def initialize(mask)
        @mask = mask
      end

      def include?(byte)
        (@mask & (1 << byte)) != 0
      end
      alias_method :===, :include?
    end

    # Thompson-style NFA compiled from a pattern AST.  Immutable once
    # sealed, therefore safe to share between sessions.
    # @!visibility private
    class Automaton
      # bytes matched by a normal capture ([^/?#]+)
      CAPTURE = ByteSet.except(0x2F, 0x3F, 0x23)
      # bytes matched by a splat (.*? - any character but newline)
      SPLAT   = ByteSet.except(0x0A)

      attr_reader :start, :accept

      def initialize
        @transitions = []
        @epsilon     = []
      end

      def add_state
        @transitions << []
        @epsilon     << []
        @transitions.size - 1
      end

      def add_transition(from, set, to)
        @transitions[from] << [set, to]
      end

      def add_epsilon(from, to)
        @epsilon[from] << to
      end

      # Precomputes epsilon closures and freezes the automaton.
      # @!visibility private
      def seal!(start, accept)
        @start, @accept = start, accept
        @closures = @epsilon.each_with_index.map { |_, state| closure(state).freeze }.freeze
        @transitions.each(&:freeze).freeze
        @epsilon.each(&:freeze).freeze
        freeze
        self
      end

      # @return [Array<Integer>] states a new session starts in
      def start_states
        @closures[@start]
      end

      # All states reachable from +active+ by consuming +byte+.
      # @return [Array<Integer>] empty if the input can never match
      def step(active, byte)
        reached = []
        active.each do |state|
          @transitions[state].each do |set, target|
            reached.concat(@closures[target]) if set.include?(byte)
          end
        end
        reached.uniq!
        reached
      end

      private

      def closure(state, seen = {}, stack = [state])
        until stack.empty?
          current = stack.pop
          next if seen[current]
          seen[current] = true
          stack.concat(@epsilon[current])
        end
        seen.keys
      end
    end

    # Compiles a pattern AST into an {Automaton}.
    # @!visibility private
    class Compiler
      def self.build(pattern)
        require 'mustermann/ast/pattern'
        new(pattern).build
      end

      def initialize(pattern)
        unless pattern.is_a?(Mustermann::AST::Pattern)
          raise UnsupportedPatternError, "#{pattern.class} patterns cannot be matched incrementally"
        end

        options = pattern.options
        raise UnsupportedPatternError, 'the :except option is not supported for incremental matching' if options[:except]
        raise UnsupportedPatternError, 'the :capture option is not supported for incremental matching' if options[:capture]

        @pattern            = pattern
        @uri_decode         = pattern.uri_decode
        @space_matches_plus = options.fetch(:space_matches_plus, true)
      end

      def build
        @automaton  = Automaton.new
        start, stop = compile_node(@pattern.to_ast)
        @automaton.seal!(start, stop)
      end

      private

      def compile_node(node)
        case node.type
        when :root, :group        then compile_sequence(node.payload)
        when :separator           then compile_literal(node.payload)
        when :char                then compile_char(node.payload)
        when :capture             then compile_capture(node)
        when :splat, :named_splat then compile_splat(node)
        when :optional            then compile_optional(node.payload)
        else
          raise UnsupportedPatternError,
                "#{node.type} nodes cannot be matched incrementally: #{@pattern.to_s.inspect}"
        end
      end

      def compile_sequence(nodes)
        start   = @automaton.add_state
        current = start
        Array(nodes).each do |node|
          first, last = compile_node(node)
          @automaton.add_epsilon(current, first)
          current = last
        end
        [start, current]
      end

      def compile_literal(string)
        start   = @automaton.add_state
        current = start
        string.each_byte do |byte|
          nxt = @automaton.add_state
          @automaton.add_transition(current, ByteSet[byte], nxt)
          current = nxt
        end
        [start, current]
      end

      # A literal character matches the character itself as well as its
      # percent-encoded variants, mirroring AST::Compiler.char_representations.
      def compile_char(char)
        compile_alternatives(char_representations(char))
      end

      def char_representations(char)
        return [char] unless @uri_decode
        if char == ' ' && @space_matches_plus
          representations(' ') + representations('+')
        else
          representations(char)
        end
      end

      def representations(char)
        escaped = uri_parser.escape(char, /./)
        [char, escaped.upcase, escaped.downcase].uniq
      end

      def uri_parser
        Mustermann::AST::Translator::URI_PARSER
      end

      def compile_alternatives(strings)
        start = @automaton.add_state
        stop  = @automaton.add_state
        strings.each do |string|
          first, last = compile_literal(string)
          @automaton.add_epsilon(start, first)
          @automaton.add_epsilon(last, stop)
        end
        [start, stop]
      end

      def compile_capture(node)
        unless node.constraint.nil? && node.qualifier.nil?
          raise UnsupportedPatternError,
                "capture constraints cannot be matched incrementally: #{@pattern.to_s.inspect}"
        end
        compile_repeat(Automaton::CAPTURE, min: 1)
      end

      def compile_splat(node)
        unless node.constraint.nil?
          raise UnsupportedPatternError,
                "splat constraints cannot be matched incrementally: #{@pattern.to_s.inspect}"
        end
        compile_repeat(Automaton::SPLAT, min: 0)
      end

      def compile_repeat(set, min:)
        start = @automaton.add_state
        stop  = @automaton.add_state
        if min.zero?
          @automaton.add_epsilon(start, stop)
          current = start
        else
          current = @automaton.add_state
          @automaton.add_transition(start, set, current)
        end
        @automaton.add_transition(current, set, current)
        @automaton.add_epsilon(current, stop)
        [start, stop]
      end

      def compile_optional(node)
        start = @automaton.add_state
        stop  = @automaton.add_state
        first, last = compile_node(node)
        @automaton.add_epsilon(start, first)
        @automaton.add_epsilon(last, stop)
        @automaton.add_epsilon(start, stop)
        [start, stop]
      end
    end

    # The outcome of feeding a chunk to, or finishing, a session.
    #
    # The +status+ is one of +:continue+, +:mismatch+, +:match+ or
    # +:incomplete+.  +params+ is only set for +:match+.
    class Result
      attr_reader :status, :params

      def initialize(status, params: nil)
        @status = status
        @params = params
      end

      # @return [Boolean] the input so far can still be extended to a match
      def continue?
        status == :continue
      end

      # @return [Boolean] the input can never match, no matter what follows
      def mismatch?
        status == :mismatch
      end

      # @return [Boolean] the input is a complete match
      def match?
        status == :match
      end

      # @return [Boolean] the input ended prematurely (mid-pattern, mid
      #   percent escape or mid UTF-8 character)
      def incomplete?
        status == :incomplete
      end

      def inspect
        "#<#{self.class.name} :#{status}#{params ? " #{params.inspect}" : nil}>"
      end
      alias_method :to_s, :inspect
    end

    # A single incremental matching session.
    #
    # Sessions never share mutable state: every session keeps its own input
    # buffer, automaton state and params.  Only the immutable automaton is
    # shared (via the pattern) between sessions of the same pattern.
    class Session
      attr_reader :pattern

      # @param [Mustermann::Pattern] pattern the pattern to match against
      # @raise [Mustermann::Incremental::UnsupportedPatternError] if the pattern cannot be matched incrementally
      def initialize(pattern)
        raise TypeError, "expected Mustermann::Pattern, got #{pattern.class}" unless pattern.is_a?(Mustermann::Pattern)
        @pattern   = pattern
        @automaton = self.class.automaton_for(pattern)
        @active    = @automaton.start_states
        @buffer    = String.new(encoding: Encoding::BINARY)
        @status    = :continue
        @result    = nil
      end

      # @!visibility private
      def self.automaton_for(pattern)
        if pattern.instance_variable_defined?(:@incremental_automaton)
          pattern.instance_variable_get(:@incremental_automaton)
        else
          automaton = Compiler.build(pattern)
          # the automaton is immutable, so sharing it between sessions is safe
          pattern.instance_variable_set(:@incremental_automaton, automaton) unless pattern.frozen?
          automaton
        end
      end

      # Feeds a chunk of the path to the session.
      #
      # @param [#to_s] chunk the next part of the input
      # @return [Mustermann::Incremental::Result] +:continue+ or +:mismatch+
      # @raise [Mustermann::Error] if the session has already been finished
      def feed(chunk)
        raise Mustermann::Error, 'cannot feed a finished session' if finished?
        return Result.new(:mismatch) if @status == :mismatch

        chunk.to_s.each_byte do |byte|
          @active = @automaton.step(@active, byte)
          @buffer << byte
          if @active.empty?
            @status = :mismatch
            break
          end
        end
        Result.new(@status)
      end

      # Feeds a chunk of the path to the session.
      # @return [Mustermann::Incremental::Session] self, for chaining
      def <<(chunk)
        feed(chunk)
        self
      end

      # Declares the input complete and computes the final result.
      #
      # @return [Mustermann::Incremental::Result] +:match+ (with params),
      #   +:incomplete+ or +:mismatch+
      def finish
        @result ||= final_result
      end

      # @return [Boolean] whether the session has been finished
      def finished?
        !@result.nil?
      end

      # @return [Symbol] +:continue+, +:mismatch+, or the finish status
      def status
        @result ? @result.status : @status
      end

      # @return [Boolean] whether more input could still lead to a match
      def continue?
        status == :continue
      end

      # @return [Boolean] whether the input can never match
      def mismatch?
        status == :mismatch
      end

      # Finishes the session and returns the params of the match.
      # @return [Hash, nil] Sinatra style params if the input matched
      def params
        finish.params
      end

      def inspect
        "#<#{self.class.name} #{pattern.to_s.inspect} #{status}>"
      end

      private

      def final_result
        return Result.new(:mismatch) if @status == :mismatch
        return Result.new(:incomplete) unless @active.include?(@automaton.accept)

        string = @buffer.dup.force_encoding(Encoding::UTF_8)
        unless string.valid_encoding?
          # A truncated multi-byte character can only be detected once the
          # input is complete; anything else invalid can never match.
          return Result.new(truncated_utf8?(string) ? :incomplete : :mismatch)
        end

        # Extract params with the pattern's own regexp, so results are
        # identical to matching the complete input in one go.
        params = @pattern.params(string)
        params ? Result.new(:match, params: params) : Result.new(:mismatch)
      end

      # Whether the string is invalid UTF-8 solely because it ends with a
      # truncated multi-byte character.
      def truncated_utf8?(string)
        bytes = string.bytes
        index = 0
        while index < bytes.size
          byte   = bytes[index]
          length = if    byte < 0x80 then 1
                   elsif byte < 0xC2 then return false
                   elsif byte < 0xE0 then 2
                   elsif byte < 0xF0 then 3
                   elsif byte < 0xF5 then 4
                   else  return false
                   end
          if index + length > bytes.size
            return bytes[(index + 1)..].all? { |b| b.between?(0x80, 0xBF) }
          end
          (1...length).each { |offset| return false unless bytes[index + offset].between?(0x80, 0xBF) }
          index += length
        end
        false
      end
    end
  end

  class Pattern
    # Creates an incremental matching session for this pattern.
    # @return [Mustermann::Incremental::Session]
    # @raise [Mustermann::Incremental::UnsupportedPatternError]
    # @see Mustermann::Incremental
    def incremental
      Incremental.new(self)
    end
  end
end
