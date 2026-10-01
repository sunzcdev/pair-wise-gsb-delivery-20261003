# frozen_string_literal: true

module Mustermann
  class Relation
    # Decides language-level relations between symbolic regular expressions
    # ({Mustermann::Relation::IR}) using Brzozowski derivatives.
    #
    # The alphabet is a finite set of {IR::Symbol} positions: ASCII 0..127, any
    # non-ASCII codepoints that appear as raw literals in either pattern, and the
    # shared +:other+ position representing all remaining non-ASCII codepoints.
    #
    # Positions with identical membership in every relevant character predicate are
    # indistinguishable and explored as a single block, which keeps the search small
    # without sacrificing exactness. Relations are decided by exploring the full
    # product automaton, never by sampling concrete URLs.
    #
    # @api private
    class Analyzer
      NEWLINE = ?\n.ord

      # Characters preferred when reconstructing a concrete evidence path.
      PRINTABLE = [?/.ord] + ('a'.ord..'z'.ord).to_a + ('A'.ord..'Z'.ord).to_a +
        ('0'.ord..'9'.ord).to_a + %w[- _ . ~].map(&:ord) + (32..126).to_a - [NEWLINE]

      # @param left [IR::Node] body IR for the left pattern (without trailing anchor)
      # @param right [IR::Node] body IR for the right pattern
      def initialize(left, right)
        # Full patterns are anchored with Ruby's \Z, which also accepts one
        # trailing newline.
        @left  = IR::F.seq(left, IR::F.z_end)
        @right = IR::F.seq(right, IR::F.z_end)
        extras = (@left.extra_positions + @right.extra_positions).uniq.sort
        @alphabet = (0..127).to_a + extras + [:other]
        @blocks = compute_blocks
      end

      attr_reader :blocks

      # Partitions the alphabet once using every character predicate appearing in
      # either pattern. Positions in the same block agree on membership in all
      # predicates, hence on the derivative of every reachable state. Using the
      # global predicate set (a superset of those seen at any individual state) is
      # still exact but only subdivides blocks further.
      def compute_blocks
        predicates = [@left, @right].flat_map { |node|
          node.each_node.to_a.select { |child| child.type == :char }.map(&:predicate)
        }.uniq

        blocks = [@alphabet]
        predicates.each do |predicate|
          blocks = blocks.flat_map do |positions|
            inside  = positions.select { |position| predicate.include?(position) }
            outside = positions - inside
            inside.empty? || outside.empty? ? [positions] : [inside, outside]
          end
        end
        blocks.map { |positions| representative(positions) }.freeze
      end

      # @return [Symbol] +:intersect+ or +:disjoint+
      def intersection
        explore
        @both_accepting.empty? ? :disjoint : :intersect
      end

      # @return [Symbol] +:subset+ or +:not_subset+
      def subset
        explore
        @left_accepting.empty? ? :subset : :not_subset
      end

      # @return [String, nil] a concrete string in both languages, or nil if disjoint
      def witness_path
        intersection
        path_to(@both_accepting)
      end

      # @return [String, nil] a concrete string in the left language but not the right
      def counterexample
        subset
        path_to(@left_accepting)
      end

      private

      # Explores the complete reachable product automaton once, recording every state
      # pair satisfying each goal condition.
      def explore
        return if @explored
        @explored = true
        @both_accepting = []
        @left_accepting = []

        start   = [@left, @right]
        queue   = [start]
        visited = { start => true }

        until queue.empty?
          state = queue.shift
          @both_accepting << state if both_accepting?(*state)
          @left_accepting << state if left_only_accepting?(*state)

          @blocks.each do |position, _character|
            next_state = step(state, position)
            next if visited.key?(next_state)
            visited[next_state] = true
            queue << next_state
          end
        end
      end

      def step(state, position)
        symbol = IR::Symbol.new(position)
        [state.first.derivative(symbol), state.last.derivative(symbol)]
      end

      def both_accepting?(left, right)
        left.nullable? && right.nullable?
      end

      def left_only_accepting?(left, right)
        left.nullable? && !right.nullable?
      end

      # Reconstructs a concrete path to one of the given target states, preferring
      # printable URL-style characters. Among equally short paths the search expands
      # printable characters before control characters.
      def path_to(targets)
        return nil if targets.empty?
        target_set = targets.to_h { |state| [state, true] }

        start   = [@left, @right]
        queue   = [[start, String.new]]
        visited = { start => true }

        until queue.empty?
          state, path = queue.shift
          return path if target_set.key?(state)

          transitions = @blocks.map do |position|
            [step(state, position), position]
          end.reject { |next_state, _| visited.key?(next_state) }

          transitions.each { |next_state, _| visited[next_state] = true }
          transitions.sort_by! { |_, position| position_rank(position) }
          transitions.each do |next_state, position|
            character = position.is_a?(Integer) ? position.chr(Encoding::UTF_8) : nil
            queue << [next_state, character.nil? ? path : path + character]
          end
        end
        nil
      end

      def position_rank(position)
        return PRINTABLE.index(position) if position.is_a?(Integer) && PRINTABLE.include?(position)
        position == NEWLINE ? 2 : 3 # :other and other control characters last
      end

      # Partitions the alphabet into blocks of positions for which the derivatives of
      # both states are identical, and returns one representative position per block.
      # Witness position for one alphabet block, preferring characters common in URL
      # paths. Blocks without an ASCII position are explored for exactness but yield
      # no concrete character.
      def representative(positions)
        ascii  = positions.select { |position| position.is_a?(Integer) && position < 128 }
        chosen = PRINTABLE.detect { |position| ascii.include?(position) }
        chosen ||= ascii.first
        chosen ||= positions.detect { |position| position.is_a?(Integer) }
        chosen || :other
      end
    end
  end
end
