# frozen_string_literal: true

module Mustermann
  class Relation
    # Symbolic regular expressions used by the {Relation} analyzer.
    #
    # Expressions are consumed through Brzozowski derivatives over a finite,
    # symbolic alphabet of single characters (see {Predicate}). This makes
    # language-level questions (emptiness, intersection emptiness and containment)
    # decidable without resorting to sampling.
    #
    # @api private
    module IR
      NEWLINE = ?\n.ord

      # A single-character symbol in the symbolic alphabet.
      #
      # Positions 0..127 correspond to ASCII codepoints, positions 128+ correspond to
      # non-ASCII codepoints that occur as raw literal characters in a pattern.
      # All other non-ASCII codepoints share the +:other+ position.
      class Symbol
        attr_reader :position

        def initialize(position)
          @position = position
        end

        def self.other
          @other ||= new(:other)
        end

        def other?
          position == :other
        end

        def character
          position.is_a?(Integer) ? position.chr(Encoding::UTF_8) : nil
        end

        def to_s
          position.is_a?(Integer) ? character.inspect : '<other>'
        end

        def eql?(other)
          other.is_a?(self.class) && other.position == position
        end
        alias == eql?

        def hash
          position.hash
        end
      end

      # A character predicate represented as a set of {Symbol} positions.
      #
      # @api private
      class Predicate < Set
        # @return [String] a concrete character accepted by the predicate, preferring
        #   characters commonly found in URL paths
        def witness
          preferred = ('a'.ord..'z'.ord).to_a + ('A'.ord..'Z'.ord).to_a +
            ('0'.ord..'9'.ord).to_a + %w[- _ . ~ /].map(&:ord) + (32..126).to_a
          position = preferred.detect { |codepoint| include?(codepoint) }
          position ||= detect { |codepoint| codepoint.is_a?(Integer) }
          position&.chr(Encoding::UTF_8)
        end

        def self.singleton(codepoint)
          new([codepoint])
        end

        def self.range(range)
          new(range.to_a)
        end

        # Predicate matching every character except the given ASCII codepoints.
        # Also includes every non-ASCII codepoint (both the shared +:other+ position
        # and the explicit literal positions in +extras+).
        def self.all_except(*codepoints, extras: [])
          forbidden = codepoints.flatten
          positions = (0..127).reject { |codepoint| forbidden.include?(codepoint) }
          new(positions + extras + [:other])
        end

        # Ruby dot without the +m+ flag: anything except ASCII newline.
        def self.dot(extras: [])
          all_except(?\n.ord, extras: extras)
        end

        def intersection(other)
          self.class.new(super)
        end
      end

      # Immutable symbolic regular expression node.
      #
      # Nodes are deduplicated through {Factory} so that state exploration can rely
      # on object identity.
      #
      # @api private
      class Node
        attr_reader :type, :children, :predicate

        # Memoized derivatives keyed by alphabet position. Product automaton
        # exploration revisits the same (node, symbol) pairs very often.
        def derivative(symbol)
          (@derivatives ||= {})[symbol.position] ||= compute_derivative(symbol)
        end

        def initialize(type, *args)
          @type = type
          if args.first.is_a?(Predicate)
            @predicate = args.first
            @children  = [].freeze
          else
            @children = args.freeze
          end
        end

        # @return [Boolean] whether the empty string is accepted
        def nullable?
          case type
          when :eps, :star, :univ, :z_end, :z_after
            true
          when :char, :empty
            false
          when :line_end
            # $ matches at end of string or immediately before a newline.
            true
          when :guarded
            # at_end=true: stop when tail matches at the end position.
            # at_end=false: the continuation after stopping is the suffix/rest
            # sequence passed as the fourth child; its nullability decides.
            if children[2] == :line_end
              children[5] == true && children[1].nullable? && children[3].nullable?
            else
              # The loop may stop here iff tail·terminator matches the empty prefix
              # and the continuation accepts from this position.
              children[5] == true && F.seq(children[1], children[2]).nullable? && children[3].nullable?
            end
          when :seq
            children.all?(&:nullable?)
          when :alt
            children.any?(&:nullable?)
          when :inter
            children.all?(&:nullable?)
          when :not
            !children.first.nullable?
          when :not_ahead
            # Negative look-ahead at the current position succeeds iff its argument
            # cannot match the empty prefix.
            !children.first.nullable?
          else
            raise Mustermann::Error, "unsupported node type #{type}"
          end
        end

        # Brzozowski derivative with respect to a concrete {Symbol}.
        # @return [Node]
        def compute_derivative(symbol)
          case type
          when :eps, :empty
            F.empty
          when :z_end
            # Ruby \Z matches at end of string or before one trailing newline.
            symbol.position == NEWLINE ? F.z_after : F.empty
          when :z_after
            # No further character may follow the trailing newline.
            F.empty
          when :char
            predicate.include?(symbol.position) ? F.eps : F.empty
          when :line_end
            # $ matches at end or before a newline; after a newline any suffix is
            # allowed, which is the universal language.
            symbol.position == NEWLINE ? F.univ : F.empty
          when :seq
            # D_c(PQ) = D_c(P)·Q ∪ (P nullable ? D_c(Q) : ∅)
            head, *tail = children
            branches = [F.seq(head.derivative(symbol), *tail)]
            branches << F.seq(*tail).derivative(symbol) if head.nullable?
            F.alt(*branches)
          when :alt
            F.alt(*children.map { |child| child.derivative(symbol) })
          when :inter
            F.inter(*children.map { |child| child.derivative(symbol) })
          when :not
            F.not(children.first.derivative(symbol))
          when :star
            F.seq(children.first.derivative(symbol), self)
          when :univ
            F.univ
          when :guarded
            guarded_derivative(symbol)
          when :not_ahead
            child = children.first
            # (?!P) at a position fails if P matches the empty prefix. Otherwise,
            # after consuming c, the surviving assertion tracks P's derivative.
            child.nullable? ? F.empty : F.not_ahead(child.derivative(symbol))
          else
            raise Mustermann::Error, "unsupported node type #{type}"
          end
        end

        # guarded(body, tail, terminator) models
        # (?:(?!tail terminator)body)+ followed externally by the optional suffix.
        #
        # The assertion is re-evaluated at every repetition but always against the
        # *original* tail (a suffix matches starting at the current position), which
        # keeps the derivative state space finite. A character is consumed through
        # body only when tail·terminator does not match the prefix at this position.
        def guarded_derivative(symbol)
          body, tail, terminator, after = children
          if terminator == :line_end
              return line_end_guarded(body, tail, after, symbol)
          end

          # at_end=false. At the current position either:
          # * the remainder starts with tail·terminator, in which case the loop
          #   stops without consuming and +after+ continues from this same position;
          # * one body character is consumed. If that same character also completes
          #   tail·terminator at the new position, the loop ends there; otherwise
          #   the prefix assertion is re-checked from scratch at the next position.
          prefix     = guarded_prefix
          progressed = prefix.derivative(symbol)
          # The assertion is re-evaluated at every position, so a prefix that stops
          # matching restarts from its initial form.
          progressed = initial_prefix.derivative(symbol) if progressed == F.empty

          if prefix.nullable?
            # tail·terminator already matches the prefix at this position.
            after.derivative(symbol)
          else
            body_next = body.derivative(symbol)
            if body_next == F.empty
              # Body cannot consume this character. If at least one body character
              # was already consumed, the repetition ends and after continues.
              children[5] == true ? after.derivative(symbol) : F.empty
            elsif progressed.nullable?
              # The character completes tail·terminator: stop with the continuation.
              F.seq(body_next, after)
            else
              state = progressed == F.empty ? nil : progressed
              F.seq(body_next, F.guarded(body, tail, terminator, after, state, true))
            end
          end
        end

        # at_end=true models (?:(?!tail$)body)+ followed by the optional tail. The
        # body stops at the end position; if the remainder begins with tail it must
        # be consumed through the continuation (suffix), never as body.
        def line_end_guarded(body, tail, after, symbol)
          prefix     = F.seq(tail, F.line_end)
          progressed = prefix.derivative(symbol)
          body_next  = body.derivative(symbol)

          branches = []
          return F.empty if body_next == F.empty
          # End the capture at the line end once at least one body character was
          # consumed; the optional suffix completes the match.
          if children[5] == true
            branches << after
          end
          # Consume a body character unless tail$ starts matching at the new spot.
          unless progressed.nullable?
            branches << F.seq(body_next,
                              F.guarded(body, tail, :line_end, after, nil, true))
          end
          F.alt(*branches)
        end

        # The stopping prefix tail·terminator at its initial position.
        def initial_prefix
          F.seq(children[1], children[2])
        end

        # Tracks the prefix as body characters are consumed; nil means initial.
        def guarded_prefix
          children[4] || initial_prefix
        end



        # Enumerates this node and every descendant.
        def each_node(&block)
          return enum_for(__method__) unless block
          yield self
          children.each { |child| child.each_node(&block) if child.is_a?(Node) }
        end

        # @return [Array<Integer>] non-ASCII literal codepoints that need their own
        #   alphabet position in addition to ASCII and +:other+
        def extra_positions
          each_node.filter_map do |node|
            next unless node.type == :char
            node.predicate.select { |position| position.is_a?(Integer) && position >= 128 }
          end.flatten.uniq
        end

        def inspect
          case type
          when :char then "[#{predicate.map(&:to_s).join(',')}]"
          when :eps  then 'eps'
          when :empty then 'empty'
          when :line_end then '$'
          when :z_end then 'Z'
          when :z_after then "Z'"
          when :univ  then '.*'
          when :not  then "not(#{children.first.inspect})"
          when :not_ahead then "not_ahead(#{children.first.inspect})"
          else
            "#{type}(#{children.map(&:inspect).join(', ')})"
          end
        end
      end

      # Canonicalizing node factory.
      #
      # @api private
      module F
        @cache  = {}
        @mutex  = Mutex.new

        def self.node(type, *args)
          @mutex.synchronize do
            @cache[[type, args]] ||= Node.new(type, *args)
          end
        end

        def self.eps
          @eps ||= node(:eps)
        end

        def self.empty
          @empty ||= node(:empty)
        end

        def self.univ
          @univ ||= node(:univ)
        end

        # Ruby \Z anchor state.
        def self.z_end
          @z_end ||= node(:z_end)
        end

        # Ruby $ anchor used inside look-aheads (zero-width).
        def self.line_end
          @line_end ||= node(:line_end)
        end

        # State after consuming the single newline allowed by \Z.
        def self.z_after
          @z_after ||= node(:z_after)
        end

        def self.char(predicate)
          return empty if predicate.empty?
          node(:char, predicate)
        end

        def self.seq(*children)
          children = children.flatten.flat_map { |child| child.type == :seq ? child.children : [child] }
          children.reject! { |child| child.type == :eps }
          return empty if children.any? { |child| child.type == :empty }
          return eps   if children.empty?
          return children.first if children.size == 1
          node(:seq, *children)
        end

        def self.alt(*children)
          children = children.flatten.flat_map { |child| child.type == :alt ? child.children : [child] }.uniq
          children.delete(empty)
          return empty if children.empty?
          return children.first if children.size == 1
          node(:alt, *children)
        end

        def self.inter(*children)
          children = children.flatten
          return empty if children.any? { |child| child.type == :empty }
          children = children.flat_map { |child| child.type == :inter ? child.children : [child] }
          children.delete(univ)
          return univ  if children.empty?
          return children.first if children.size == 1
          node(:inter, *children)
        end

        def self.opt(child)
          alt(eps, child)
        end

        def self.star(child)
          return eps if child.type == :empty || child.type == :eps
          return child if child.type == :star
          node(:star, child)
        end

        def self.plus(child)
          seq(child, star(child))
        end

        # Exactly +min+ and at most +max+ repetitions (+max+ may be nil).
        def self.repeat(child, min, max)
          fixed = Array.new(min) { child }
          return seq(*fixed) if max == min
          optional = if max.nil?
            star(child)
          else
            alt(*(0..(max - min)).map { |count| seq(*Array.new(count) { child }) })
          end
          seq(*fixed, optional)
        end

        def self.not(child)
          return empty if child.type == :univ
          return univ  if child.type == :empty
          node(:not, child)
        end

        def self.not_ahead(child)
          node(:not_ahead, child)
        end

        # @see Node#guarded_derivative
        # @param after [Node] what follows once the repetition stops
        # @param prefix_state [Node, nil] internal derivative of tail·terminator
        # @param entered [Boolean] whether at least one body character was consumed
        def self.guarded(body, tail, terminator, after = empty, prefix_state = nil, entered = false)
          node(:guarded, body, tail, terminator, after, prefix_state, entered)
        end

      end

    end
  end
end



