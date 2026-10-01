# frozen_string_literal: true
require 'strscan'

module Mustermann
  module Relation
    # Compiles the *small, restricted* subset of regular expressions produced
    # by Mustermann's own pattern compiler into {NFA} fragments.
    #
    # This is intentionally not a general Ruby regexp engine.  Anchors,
    # look(arounds|behinds), backreferences and similar features raise
    # {Unsupported}, letting the relation analyzer answer "unknown" instead of
    # guessing.
    #
    # Supported: literals (with URI percent-encoding expansion), +.+,
    # character classes (incl. ranges, +\d+/\w+ and +[[:posix:]]+),
    # non-capturing and atomic groups, alternation and the quantifiers
    # +*+ + + +? + +{n}+ + +{n,}+ + +{n,m}+ (greedy, lazy or possessive).
    #
    # @api private
    class RegexpCompiler
      # Raised when a fragment uses a regex feature the analyzer cannot model.
      class Unsupported < StandardError
      end

      def self.compile(graph, source, space_matches_plus: true)
        new(graph, source, space_matches_plus).compile
      end

      def initialize(graph, source, space_matches_plus)
        @graph               = graph
        @scanner             = ::StringScanner.new(source)
        @space_matches_plus  = space_matches_plus
      end

      def compile
        fragment = alternation
        raise Unsupported, "unbalanced parentheses" unless @scanner.eos?
        fragment
      end

      private

      def alternation
        branches = [sequence]
        branches << sequence while accept(?|)
        branches.size == 1 ? branches.first : @graph.union_fragment(branches)
      end

      def sequence
        parts = []
        parts << quantifier(atom) until @scanner.eos? || peek == ?) || peek == ?|
        parts.empty? ? @graph.empty_fragment : @graph.concat_fragment(*parts)
      end

      def atom
        char = getch
        case char
        when ?(  then group
        when ?[  then character_class
        when ?.  then @graph.char_fragment(Predicates::DOT)
        when ?\\ then escape
        when ?^, ?$
          raise Unsupported, "anchors are not supported"
        when ?*, ?+, ??, ?), ?|
          raise Unsupported, "unexpected #{char.inspect}"
        else single_literal(char)
        end
      end

      def group
        if accept(??)
          marker = getch
          case marker
          when ?: then # non-capturing group
          when ?> then # atomic group: same language as a normal group
          else raise Unsupported, "unsupported group marker #{marker.inspect}"
          end
        end
        body = alternation
        expect(?))
        body
      end

      def escape
        char = getch
        case char
        when ?d then @graph.char_fragment(Predicates::DIGIT)
        when ?D then @graph.char_fragment(Predicates::NON_DIGIT)
        when ?w then @graph.char_fragment(Predicates::WORD)
        when ?W then @graph.char_fragment(Predicates::NON_WORD)
        when ?s, ?S
          raise Unsupported, "whitespace classes are not used by Mustermann"
        when '0'..'9'
          raise Unsupported, "backreferences/octal escapes not supported"
        else single_literal(char)
        end
      end

      # One fragment matching one raw input character position, expanding the
      # literal into its percent-encoded representations.
      def single_literal(char)
        representations = Predicates.encodings(char,
          space_matches_plus: @space_matches_plus)
        alternatives = representations.map do |representation|
          pieces = representation.each_char.map do |piece|
            @graph.char_fragment(Predicates.exact(piece))
          end
          @graph.concat_fragment(*pieces)
        end
        @graph.union_fragment(alternatives)
      end

      def character_class
        negate = accept(?^)
        members = []
        members << class_member until peek == ?]
        expect(?])
        @graph.char_fragment(class_predicate(members, negate))
      end

      # A member is one of:
      #   [:char, c]               literal character
      #   [:range, from, to]       character range
      #   [:predicate, #call]      \d, \w or [[:posix:]]
      def class_member
        # A '-' is a literal when it starts or ends the class body.
        return [:char, getch] if peek == ?- &&
          (@scanner.rest[1].nil? || @scanner.rest[1] == ?])

        first = class_unit
        if range_operator?(first)
          getch
          second = class_unit
          raise Unsupported, "class escape in range" unless second[0] == :char
          return [:range, first[1], second[1]]
        end
        first
      end

      # Whether the next characters form a '-' range operator (not an escaped
      # '\-' and not a trailing literal '-').
      def range_operator?(first)
        return false unless first[0] == :char
        rest = @scanner.rest
        return false if rest[1].nil? || rest[1] == ?]
        rest[0] == ?- && rest[1] != ?\\
      end

      def class_unit
        if @scanner.match?(/\[\:/)
          @scanner.pos += 2
          name = @scanner.scan(/[a-z]+/) or raise Unsupported, "bad POSIX class"
          @scanner.scan(/:\]/) or raise Unsupported, "bad POSIX class"
          return [:predicate, Predicates.posix(name)]
        end
        char = getch
        return [:char, char] unless char == ?\\
        escaped = getch
        return [:predicate, Predicates::DIGIT] if escaped == ?d
        return [:predicate, Predicates::WORD]  if escaped == ?w
        raise Unsupported, "whitespace classes are not used by Mustermann" if escaped == ?s
        # An escaped punctuation character (e.g. "\/", "\.") denotes that
        # literal character, not a backslash followed by it.
        [:char, escaped]
      end

      def class_predicate(members, negate)
        positive = proc do |char|
          members.any? do |kind, one, two|
            case kind
            when :char      then one == char
            when :range     then char >= one && char <= two
            when :predicate then one.call(char)
            end
          end
        end
        negate ? proc { |char| !positive.call(char) } : positive
      end

      def quantifier(fragment)
        case peek
        when ?* then bounded(fragment, 0, nil)
        when ?+ then bounded(fragment, 1, nil)
        when ?? then bounded(fragment, 0, 1)
        when ?{ then explicit_bounds(fragment)
        else fragment
        end
      end

      def bounded(fragment, min, max)
        getch
        getch if peek == ?? || peek == ?+ # lazy/possessive marker
        @graph.repeat_fragment(fragment, min, max)
      end

      def explicit_bounds(fragment)
        position = @scanner.pos
        getch
        min = integer
        max = min
        if accept(?,)
          max = peek == ?} ? nil : integer
        end
        if accept(?}) && min && (max.nil? || max >= min)
          getch if peek == ?? || peek == ?+
          return @graph.repeat_fragment(fragment, min, max)
        end
        @scanner.pos = position
        fragment
      end

      def integer
        digits = @scanner.scan(/\d+/)
        digits && Integer(digits)
      end

      def peek
        @scanner.eos? ? nil : @scanner.peek(1)
      end

      def getch
        raise Unsupported, "unexpected end of fragment" if @scanner.eos?
        @scanner.getch
      end

      def accept(char)
        return unless peek == char
        @scanner.getch
        true
      end

      def expect(char)
        getch == char or raise Unsupported, "expected #{char.inspect}"
      end
    end
  end
end
