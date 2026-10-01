# frozen_string_literal: true
require 'uri'

module Mustermann
  module Relation
    # Character predicates used on {NFA} edges.
    #
    # Every predicate answers whether one *raw* character of an input string
    # is accepted.  Percent-encoded literals are expanded into a union of the
    # literal character and its upper/lower case encodings, mirroring
    # {Mustermann::AST::Compiler#encoded} so that strings matching the real
    # pattern are also accepted by the analysis model (and vice versa).
    #
    # @api private
    module Predicates
      # A splat consumes any character except a newline (Regexp +.+/.+ semantics).
      DOT = proc { |char| char != "\n" }

      DIGIT     = proc { |char| ('0'..'9').cover?(char) }
      NON_DIGIT = proc { |char| !DIGIT.call(char) }

      WORD     = proc { |char| char =~ /\w/ }
      NON_WORD = proc { |char| !WORD.call(char) }

      # Predicate for a POSIX character class, evaluated with Ruby's own
      # POSIX classes so Unicode behavior matches the compiled pattern.
      def self.posix(name)
        @posix ||= {}
        @posix[name] ||= begin
          regexp = Regexp.new("[[:#{name}:]]")
          proc { |char| !!(char =~ regexp) }
        end
      end

      # @param [String] char a single character
      # @return [Proc] predicate matching that exact character
      def self.exact(char)
        proc { |other| other == char }
      end

      # Union of several predicates.
      def self.any_of(predicates)
        proc { |char| predicates.any? { |predicate| predicate.call(char) } }
      end

      # Negation of a predicate.
      def self.not(predicate)
        proc { |char| !predicate.call(char) }
      end

      # Raw character representations for a pattern literal (the character
      # itself plus upper/lower percent encodings), matching the real
      # compiler's URI-decoding behavior.
      #
      # @return [Array<String>]
      def self.encodings(char, space_matches_plus: true)
        @encodings ||= {}
        @encodings[[char, space_matches_plus]] ||=
          if char == ' ' && space_matches_plus
            encodings(' ', space_matches_plus: false) +
              encodings('+', space_matches_plus: false)
          else
            escaped = uri_parser.escape(char, /./)
            [char, escaped.upcase, escaped.downcase].uniq
          end
      end

      def self.uri_parser
        URI::RFC2396_PARSER
      end

    end
  end
end
