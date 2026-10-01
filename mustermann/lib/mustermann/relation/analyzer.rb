# frozen_string_literal: true

module Mustermann
  module Relation
    # Decides how the languages of two patterns relate by exploring the
    # product of their epsilon-NFAs over a concrete alphabet.
    #
    # The search is a subset construction on the product: each state is a pair
    # of *sets* of NFA states (determinized on both sides).  It records
    #
    #   * a product accepting state  -> a common string (intersection),
    #   * a state where the left NFA accepts but the right one does not
    #     -> a string in the left language but not the right (counterexample
    #        to "left is contained in right").
    #
    # Because every transition label is a single-character predicate and all
    # predicates are derived from a small, ASCII-closed set of character
    # classes, enumerating ASCII 0..127 suffices to discover every reachable
    # product state: any two predicates with a non-empty intersection share an
    # ASCII member.  This is an exhaustive proof over the languages, not a
    # sample-based guess.
    #
    # @api private
    class Analyzer
      # Sentinel set: the automaton has no reachable state at this point.
      DEAD = [:__dead__].freeze

      Result = Struct.new(:intersect?, :sub_set?, :witness, :counterexample,
        :left_nfa, :right_nfa, keyword_init: true)

      def self.analyze(left, right)
        analyze_nfas(Builder.build(left), Builder.build(right))
      end

      def self.analyze_nfas(left_nfa, right_nfa)
        allocate.send(:run_product, left_nfa, right_nfa)
      end

      private

      # Breadth-first search over determinized product states.
      def run_product(left_nfa, right_nfa)
        alphabet = (NFA::ALPHABET + left_nfa.extra_symbols +
                    right_nfa.extra_symbols).uniq
        start = [left_nfa.closure([left_nfa.start]).sort,
                 right_nfa.closure([right_nfa.start]).sort]

        queue    = [start]
        parent   = { start => nil }
        symbol   = { start => nil }
        accepting_witness     = nil
        not_contained_witness = nil

        until queue.empty?
          states = queue.shift
          left_states, right_states = states

          left_accepts  = accepting?(left_nfa, left_states)
          right_accepts = accepting?(right_nfa, right_states)

          if left_accepts && right_accepts
            accepting_witness ||= states
          end
          if left_accepts && !right_accepts
            not_contained_witness ||= states
          end

          # Once both have been found we can stop early.
          break if accepting_witness && not_contained_witness

          successors(left_nfa, right_nfa, left_states, right_states, alphabet).each do
            |next_states, char|
            next if parent.key?(next_states)
            parent[next_states] = states
            symbol[next_states] = char
            queue << next_states
          end
        end

        contained = not_contained_witness.nil?
        Result.new(
          intersect?:      !!accepting_witness,
          sub_set?:        contained,
          witness:         accepting_witness && path(parent, symbol, accepting_witness),
          counterexample:  not_contained_witness &&
                             path(parent, symbol, not_contained_witness),
          left_nfa:        left_nfa,
          right_nfa:       right_nfa)
      end

      def accepting?(nfa, states)
        states != DEAD && states.any? { |state| nfa.accept?(state) }
      end

      # Enumerates distinct successor product states for one input character.
      def successors(left_nfa, right_nfa, left_states, right_states, alphabet)
        grouped = {}
        alphabet.each do |char|
          next_left  = step_state(left_nfa, left_states, char)
          next_right = step_state(right_nfa, right_states, char)
          # A dead *left* cannot contribute any string; a dead *right* is kept
          # so a string the left accepts but the right rejects is discovered.
          next if next_left == DEAD
          pair = [next_left, next_right]
          grouped[pair] ||= char
        end
        grouped
      end

      # Deterministic successor set, using the {DEAD} sentinel once no NFA
      # state can be reached.
      def step_state(nfa, states, char)
        return DEAD if states == DEAD
        reached = nfa.closure(nfa.step(states, char))
        reached.empty? ? DEAD : reached.sort
      end

      # Reconstructs the input string leading to a product state.
      def path(parent, symbol, state)
        chars = []
        while (char = symbol[state])
          chars << char
          state = parent[state]
        end
        chars.reverse.join
      end
    end
  end
end
