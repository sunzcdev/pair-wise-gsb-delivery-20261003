# frozen_string_literal: true

module Mustermann
  module Relation
    # Turns a raw witness produced by the product search into a readable,
    # printable path without changing which patterns it satisfies.
    #
    # The analyzer labels transitions only with ASCII code points and tends to
    # pick the first one that is accepted by both sides (often a control
    # character).  Such a string is a valid mathematical witness, but users
    # want something they can paste into a request.  This replaces each
    # character with a friendly one that keeps both NFAs in the same product
    # state, which guarantees the resulting string is still recognized.
    #
    # @api private
    module Witness
      # Prefer these characters when looking for a human-readable witness.
      FRIENDLY = ('a'..'z').to_a + ('0'..'9').to_a +
        ('A'..'Z').to_a + ['-', '_', '~', '.'].freeze

      # Fallback symbols accepted by permissive predicates (e.g. splats).
      SAFE_SYMBOLS = ['x', 'a', '0', '-', '_', '~', '.', 'n', 'q'].freeze

      # Printable punctuation that distinguishes permissive predicates from
      # narrower ones (e.g. "@" is allowed by a Sinatra capture but not by a
      # URI-template variable).
      EXTRA_PRINTABLE = ['@', '!', '$', '&', "'", '(', ')', '*', '+', ',',
        ';', '=', ':', '#', '?'].freeze

      # Raw characters used inside patterns as separators that should be
      # preserved verbatim where possible.
      PUNCTUATION = ['/', '.', '@', ',', ';', '?', '&', '#', '=', '+'].freeze

    def self.beautify(raw, left_nfa, right_nfa)
      return raw if raw.empty?
        # Anything that is not a "nice" character is reconsidered.
        ugly = raw.each_char.any? { |char| !nice?(char) }
        return raw unless ugly

        rebuilt = String.new
        left_states  = left_nfa.closure([left_nfa.start])
        right_states = right_nfa.closure([right_nfa.start])

        raw.each_char do |original|
          next_left  = advance(left_nfa, left_states, original)
          next_right = advance(right_nfa, right_states, original)

          replacement = choose(original, left_nfa, right_nfa,
            left_states, right_states, next_left, next_right)

          rebuilt << replacement
          left_states  = next_left
          right_states = next_right
        end
        rebuilt
      end

      def self.advance(nfa, states, char)
        return Analyzer::DEAD if states == Analyzer::DEAD
        reached = nfa.closure(nfa.step(states, char))
        reached.empty? ? Analyzer::DEAD : reached.sort
      end

      def self.choose(original, left_nfa, right_nfa,
                     left_states, right_states, target_left, target_right)
        # Preserve structural punctuation exactly; otherwise prefer a friendly
        # alphanumeric representative.
        candidates = STRUCTURAL.include?(original) ?
          [original] : (FRIENDLY + [original])

        # Control characters produced by the search are replaced by *any*
        # printable character that lands in the same product state.
        candidates = FRIENDLY + EXTRA_PRINTABLE if control?(original)

        pick = candidates.find do |char|
          advance(left_nfa, left_states, char) == target_left &&
            advance(right_nfa, right_states, char) == target_right
        end
        return pick if pick

        # No alphanumeric representative reaches the same product state (this
        # happens with very permissive predicates such as splats combined with
        # a specific continuation). Search for any printable representative
        # that keeps the final witness acceptable to both real matchers.
        printable_alternative(left_nfa, right_nfa,
          left_states, right_states, target_left, target_right) || original
      end

      def self.control?(char)
        char < ' ' || char == "\x7f"
      end

      def self.printable_alternative(left_nfa, right_nfa,
                                    left_states, right_states, target_left,
                                    target_right)
        (SAFE_SYMBOLS + EXTRA_PRINTABLE).find do |char|
          advance(left_nfa, left_states, char) == target_left &&
            advance(right_nfa, right_states, char) == target_right
        end
      end

      # Characters kept verbatim even in a beautified witness: path and
      # expression separators that are part of the pattern skeleton.
      STRUCTURAL = ['/', '.', ',', ';', '?', '&', '#', '='].freeze

      def self.nice?(char)
        return true if STRUCTURAL.include?(char)
        char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
          char >= '0' && char <= '9' || char == '-' || char == '_' || char == '~'
      end
    end
  end
end
