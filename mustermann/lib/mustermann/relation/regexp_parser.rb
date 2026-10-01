# frozen_string_literal: true

module Mustermann
  class Relation
    # Minimal recursive-descent parser turning the small, trusted regular
    # expressions shipped with Mustermann's built-in capture converters
    # (Integer, Float, Date, Gem::Version, :slug, :uuid, :locale) into {IR}.
    #
    # Arbitrary user supplied Regexp/string constraints are intentionally not
    # parsed here; they are reported as unknown instead.
    #
    # @api private
    class RegexpParser
      # @param source [String] regular expression source
      # @return [IR::Node]
      def self.parse(source)
        new(source).parse
      end

      def initialize(source)
        @source = source
        @pos    = 0
      end

      def parse
        node = read_alternative
        raise Mustermann::Error, "unexpected input at #{@pos}: #{@source[@pos..].inspect}" unless eos?
        node
      end

      private

      def read_alternative
        branches = [read_sequence]
        while accept(?|)
          branches << read_sequence
        end
        IR::F.alt(*branches)
      end

      def read_sequence
        nodes = []
        nodes << read_quantified while !eos? && @source[@pos] != ?| && @source[@pos] != ?)
        IR::F.seq(*nodes)
      end

      def read_quantified
        node = read_atom
        while (quantifier = peek_quantifier)
          advance
          case quantifier
          when ?? then node = IR::F.opt(node)
          when ?* then node = IR::F.star(node)
          when ?+ then node = IR::F.plus(node)
          when ?{ then node = read_bounded(node)
          end
        end
        node
      end

      def peek_quantifier
        return if eos?
        char = @source[@pos]
        char if char == ?? || char == ?* || char == ?+ || char == ?{
      end

      def read_bounded(node)
        min = read_integer
        max = min
        if accept(?,)
          max = eos? || @source[@pos] == ?} ? nil : read_integer
        end
        expect(?})
        max.nil? ? IR::F.seq(node, IR::F.star(node)) : IR::F.repeat(node, min, max)
      end

      def read_integer
        start = @pos
        advance while !eos? && @source[@pos] =~ /[0-9]/
        raise Mustermann::Error, "expected number in #{@source.inspect}" if @pos == start
        @source[start...@pos].to_i
      end

      def read_atom
        case char = advance
        when ?(
          if @source[@pos] == ??
            # (?:...) non-capturing and (?>...) atomic groups; the latter has the
            # same language as an ordinary group here.
            advance
            advance if @source[@pos] == ?: || @source[@pos] == ?>
          end
          node = read_alternative
          expect(?))
          node
        when ?\\
          read_escape
        when ?[
          read_character_class
        when ?.
          IR::F.char(IR::Predicate.dot)
        else
          literal(char)
        end
      end

      def read_escape
        char = advance
        case char
        when 'd' then digit
        when 'D' then IR::F.char(IR::Predicate.all_except(*digit_positions))
        when 'w' then word
        when 'W' then IR::F.char(IR::Predicate.all_except(*word_positions))
        when 's' then whitespace
        when 'S' then IR::F.char(IR::Predicate.all_except(*whitespace_positions))
        when 'n' then literal("\n")
        when 't' then literal("\t")
        when 'r' then literal("\r")
        else literal(char)
        end
      end

      def read_character_class
        negated = accept(?^)
        positions = []
        until eos? || @source[@pos] == ?]
          first = read_class_character
          if @source[@pos] == ?- && @source[@pos + 1] != ?]
            advance
            second = read_class_character
            positions.concat((first..second).to_a)
          else
            positions.concat(Array(first))
          end
        end
        expect(?])
        predicate = negated ? IR::Predicate.all_except(*positions) : IR::Predicate.new(positions)
        IR::F.char(predicate)
      end

      def read_class_character
        if @source[@pos] == ?\\
          advance
          escaped = advance
          case escaped
          when 'd' then return digit_positions
          when 'w' then return word_positions
          when 's' then return whitespace_positions
          when 'n' then return "\n".ord
          when 't' then return "\t".ord
          when 'r' then return "\r".ord
          else return escaped.ord
          end
        end
        advance&.ord
      end

      def digit
        IR::F.char(IR::Predicate.new(digit_positions))
      end

      def word
        IR::F.char(IR::Predicate.new(word_positions))
      end

      def whitespace
        IR::F.char(IR::Predicate.new(whitespace_positions))
      end

      def digit_positions
        ('0'.ord..'9'.ord).to_a
      end

      def word_positions
        ('a'.ord..'z'.ord).to_a + ('A'.ord..'Z'.ord).to_a + ('0'.ord..'9'.ord).to_a + [?_.ord]
      end

      def whitespace_positions
        [" ", "\t", "\n", "\r", "\v", "\f"].map(&:ord)
      end

      def literal(char)
        IR::F.char(IR::Predicate.singleton(char.ord))
      end

      def eos?
        @pos >= @source.length
      end

      def peek
        @source[@pos]
      end

      def advance
        char = @source[@pos]
        @pos += 1 if char
        char
      end

      def accept(expected)
        return false unless @source[@pos] == expected
        @pos += 1
        true
      end

      def expect(expected)
        raise Mustermann::Error, "expected #{expected.inspect} in #{@source.inspect}" unless accept(expected)
      end
    end
  end
end
