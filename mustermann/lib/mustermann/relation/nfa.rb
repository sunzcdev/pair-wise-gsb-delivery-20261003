# frozen_string_literal: true

module Mustermann
  module Relation
    # Internal, intentionally small epsilon-NFA used for static pattern
    # relation analysis (intersection and containment).
    #
    # All fragments share one graph of globally numbered states, which keeps
    # concatenation/union/repetition a matter of adding epsilon edges instead
    # of copying states around.
    #
    # Transitions are labeled with predicates answering whether a *single*
    # character is allowed.  Only the ASCII range (0..127) is enumerated when
    # deciding which symbols can fire, which is sufficient because every
    # character class produced for a {Mustermann::Pattern} is either ASCII-only
    # or a superset of an ASCII class (e.g. +.*+ for a splat); hence any
    # non-empty intersection of two such predicates contains an ASCII member.
    #
    # @api private
    class NFA
      # Characters considered when searching for common symbols.
      ALPHABET = (0..127).map(&:chr).freeze

      # Extra (e.g. non-ASCII) literal characters contributed by a pattern,
      # so product exploration does not falsely call such literals disjoint.
      attr_reader :extra_symbols

      def add_extra_symbols(chars)
        (@extra_symbols ||= []).concat(Array(chars))
      end

      # A connected subgraph with one entry and one exit state.
      class Fragment
        attr_reader :entry, :exit

        def initialize(graph, entry, exit_state)
          @graph, @entry, @exit = graph, entry, exit_state
        end

        def connect_to(other)
          @graph.epsilon(@exit, other.entry)
          Fragment.new(@graph, @entry, other.exit)
        end
      end

      attr_reader :start

      def initialize
        @edges       = Hash.new { |hash, state| hash[state] = [] }
        @counter     = -1
        @accepting   = {}
        @start       = state!
        @extra_symbols = []
      end

      # @return [Integer] a fresh state
      def state!
        @counter += 1
      end

      def accept!(state)
        @accepting[state] = true
      end

      def accept?(state)
        !!@accepting[state]
      end

      # Adds an epsilon (zero-length) transition.
      def epsilon(from, to)
        @edges[from] << [nil, to]
      end

      # Adds a transition consuming one character accepted by +predicate+.
      def char_edge(from, to, predicate)
        @edges[from] << [predicate, to]
      end

      # Builds a fragment matching a single character accepted by +predicate+.
      def char_fragment(predicate)
        entry, exit_state = state!, state!
        char_edge(entry, exit_state, predicate)
        Fragment.new(self, entry, exit_state)
      end

      # Builds an empty (epsilon-only) fragment.
      def empty_fragment
        state = state!
        Fragment.new(self, state, state)
      end

      # Concatenates fragments.
      def concat_fragment(*fragments)
        fragments = fragments.compact
        return empty_fragment if fragments.empty?
        fragments.reduce do |left, right|
          left.connect_to(right)
        end
      end

      # Builds a fragment for the union of several fragments.
      def union_fragment(fragments)
        entry, exit_state = state!, state!
        fragments.each do |fragment|
          epsilon(entry, fragment.entry)
          epsilon(fragment.exit, exit_state)
        end
        Fragment.new(self, entry, exit_state)
      end

      # Wraps a fragment in an optional group.
      def optional_fragment(fragment)
        union_fragment([fragment, empty_fragment])
      end

      # Repeats a fragment at least +min+ and at most +max+ (+nil+ infinity).
      def repeat_fragment(fragment, min, max = nil)
        if max == 1 && min <= 1
          return min.zero? ? optional_fragment(fragment) : copy_fragment(fragment)
        end

        result = nil
        min.times do
          result = result ? result.connect_to(copy_fragment(fragment)) :
                             copy_fragment(fragment)
        end

        if max.nil?
          # (fragment)* implemented as a fresh state with an epsilon loop
          # through one copy of the fragment.
          entry, exit_state = state!, state!
          copy = copy_fragment(fragment)
          epsilon(entry, copy.entry)
          epsilon(entry, exit_state)
          epsilon(copy.exit, copy.entry)
          epsilon(copy.exit, exit_state)
          loop_part = Fragment.new(self, entry, exit_state)
          result = result ? result.connect_to(loop_part) : loop_part
        else
          (max - min).times do
            optional_part = optional_fragment(copy_fragment(fragment))
            result = result ? result.connect_to(optional_part) : optional_part
          end
        end
        result
      end

      # Duplicates a fragment's edges under fresh states and returns the copy.
      def copy_fragment(fragment)
        # A standalone fragment is closed: its exit is a sink, and from its
        # entry the exit is the only way to leave the fragment.
        map     = { fragment.entry => state!, fragment.exit => state! }
        seen    = { fragment.entry => true }
        pending = [fragment.entry]
        until pending.empty?
          source = pending.pop
          next if source == fragment.exit
          @edges[source].each do |predicate, target|
            target_copy = map[target] ||= state!
            unless seen[target] || target == fragment.exit
              seen[target] = true
              pending << target
            end
            predicate ? char_edge(map[source], target_copy, predicate)
                      : epsilon(map[source], target_copy)
          end
        end
        Fragment.new(self, map[fragment.entry], map.fetch(fragment.exit))
      end

      # Marks a fragment as spanning the whole pattern.
      def finish!(fragment)
        epsilon(@start, fragment.entry)
        accept!(fragment.exit)
      end

      # Epsilon closure of a set of states.
      def closure(states)
        pending = Array(states)
        seen    = {}
        until pending.empty?
          state = pending.pop
          next if seen[state]
          seen[state] = true
          @edges[state].each do |predicate, target|
            pending << target if predicate.nil?
          end
        end
        seen.keys
      end

      # States reachable from +states+ by consuming +char+.
      def step(states, char)
        targets = []
        states.each do |state|
          @edges[state].each do |predicate, target|
            targets << target if predicate && predicate.call(char)
          end
        end
        closure(targets)
      end

      # True if the NFA recognizes the complete string.
      def match?(string)
        states = closure([@start])
        string.each_char do |char|
          states = step(states, char)
          return false if states.empty?
        end
        states.any? { |state| accept?(state) }
      end

      # Copies every state and edge of +other+ into this graph under fresh
      # state numbers, returning a map describing the offsets.
      #
      # @return [Hash] +start+ and +accepts+ arrays of mapped states
      def merge_from(other)
        shift   = @counter + 1
        @counter = shift + other.instance_variable_get(:@counter)
        add_extra_symbols(other.extra_symbols)

        map = Hash.new { |hash, state| hash[state] = state + shift }
        other.instance_variable_get(:@edges).each do |state, edges|
          edges.each do |predicate, target|
            if predicate
              char_edge(map[state], map[target], predicate)
            else
              epsilon(map[state], map[target])
            end
          end
        end

        accepts = other.instance_variable_get(:@accepting).keys.map { |state| map[state] }
        accepts.each { |state| accept!(state) }
        { start: map[other.start], accepts: accepts }
      end
    end
  end
end
