# frozen_string_literal: true

module Dry
  module Monads
    # A lazy list monad with fair (interleaving) `bind` and `apply`.
    #
    # Unlike {List}, which wraps a finite array and evaluates eagerly,
    # `LazyList` is built on memoized thunks: elements are computed only
    # when demanded, so sources may be infinite. `bind` interleaves the
    # resulting streams instead of concatenating them, which guarantees
    # that every combination sitting at a finite position is eventually
    # produced, even when the first source never ends. No background
    # threads are used; everything runs on the consumer's thread.
    #
    # @example fair combination of two infinite sources
    #   naturals = LazyList.enumerate((0..).each)
    #   naturals.bind { |x| naturals.fmap { |y| [x, y] } }.take(4)
    #   # => [[0, 0], [1, 0], [0, 1], [2, 0]]
    class LazyList
      include Enumerable

      class << self
        # Builds a lazy list from finite values.
        #
        # @param values [Array<Object>] List elements
        # @return [LazyList]
        def [](*values)
          from_array = nil
          from_array = lambda do |index|
            new do
              if index < values.size
                [values[index], from_array.(index + 1)]
              end
            end
          end
          from_array.(0)
        end

        # The empty lazy list.
        #
        # @return [LazyList]
        def empty = new { nil }

        # Wraps a single value.
        #
        # @param value [Object] any object
        # @return [LazyList]
        def pure(value = Undefined, &block)
          self[Undefined.default(value, block)]
        end

        # Builds a lazy list pulling values from an enumerator on demand.
        # Every `#next` call on the enumerator happens only when the
        # consumer reaches the corresponding element.
        #
        # @param enum [Enumerator] Source enumerator
        # @return [LazyList]
        def enumerate(enum)
          new do
            begin
              [enum.next, enumerate(enum)]
            rescue StopIteration
              nil
            end
          end
        end

        # Coerces a value to a lazy list. `nil` becomes an empty list,
        # arrays are wrapped, other enumerables are pulled lazily.
        #
        # @param value [Object] Value
        # @return [LazyList]
        def coerce(value)
          case value
          when LazyList
            value
          when nil
            empty
          else
            if value.respond_to?(:to_ary)
              self[*value.to_ary]
            elsif value.respond_to?(:each)
              enumerate(value.each)
            else
              raise TypeError, "Can't coerce #{value.inspect} to LazyList"
            end
          end
        end

        # Lazily builds a list from a block returning Maybe values.
        # The block is called once per demanded element.
        #
        # @param state [Object] Initial state
        # @return [LazyList]
        def unfold(state, &block)
          new do
            m = block.(state)

            if m.some?
              new_state, x = m.value!
              [x, unfold(new_state, &block)]
            end
          end
        end
      end

      # @api private
      def initialize(&thunk)
        @thunk = thunk
        @forced = false
        @node = nil
      end

      # Fair monadic bind. The block must return a value coercible to
      # a lazy list. Result streams are interleaved so that no branch
      # can starve the others, even if it is infinite.
      #
      # @example
      #   LazyList[1, 2].bind { |x| LazyList[x, -x] } # => LazyList[1, 2, -1, -2]
      #
      # @param args [Array<Object>] arguments passed to the block or callable
      # @return [LazyList]
      def bind(*args, &block)
        fn =
          if block
            ->(x) { LazyList.coerce(block.(x, *args)) }
          else
            obj, *rest = args
            ->(x) { LazyList.coerce(obj.(x, *rest)) }
          end

        LazyList.new do
          current = node

          if current
            head, tail = current
            fn.(head).interleave(tail.bind(&fn)).node
          end
        end
      end

      # Lazily maps a block over the list.
      #
      # @param args [Array<Object>] arguments passed to the block or callable
      # @return [LazyList]
      def fmap(*args, &block)
        fn =
          if block
            ->(x) { block.(x, *args) }
          else
            obj, *rest = args
            ->(x) { obj.(x, *rest) }
          end

        LazyList.new do
          current = node

          if current
            head, tail = current
            [fn.(head), tail.fmap(&fn)]
          end
        end
      end
      alias_method :map, :fmap

      # Fairly applies the stored functions to the elements of the
      # given list. Both the functions and the arguments may be infinite.
      #
      # @param list [LazyList]
      # @return [LazyList]
      def apply(list = Undefined, &)
        v = Undefined.default(list, &)
        fmap(Curry).bind { |f| v.fmap { f.(_1) } }
      end

      # Fair concatenation: alternates elements of the two lists so
      # that an infinite list cannot starve the other one.
      #
      # @param other [LazyList]
      # @return [LazyList]
      def interleave(other)
        LazyList.new do
          current = node

          if current
            head, tail = current
            [head, other.interleave(tail)]
          else
            other.node
          end
        end
      end

      # Ordered lazy concatenation. Note that if the receiver is
      # infinite, elements of the other list are never reached;
      # use {#interleave} when fairness is required.
      #
      # @param other [LazyList]
      # @return [LazyList]
      def +(other)
        other = LazyList.coerce(other)

        LazyList.new do
          current = node

          if current
            head, tail = current
            [head, tail + other]
          else
            other.node
          end
        end
      end

      # Takes the first n elements. Only the elements required to
      # produce the result are forced.
      #
      # @param n [Integer]
      # @return [Array]
      def take(n)
        taken = []
        current = self

        while taken.size < n && !current.empty?
          taken << current.first
          current = current.tail
        end

        taken
      end

      # Returns the first element, forcing only the head.
      #
      # @return [Object]
      def first = node&.first

      # Returns the first element wrapped with a `Maybe`.
      #
      # @return [Maybe<Object>]
      def head = Monads::Maybe.coerce(first)

      # Returns the lazy tail of the list.
      #
      # @return [LazyList]
      def tail
        current = node
        current ? current.last : self
      end

      # Whether the list is empty. Forces at most one element.
      #
      # @return [TrueClass, FalseClass]
      def empty? = node.nil?

      # Iterates over the list. Beware: this never stops for
      # infinite lists unless the block throws or raises.
      #
      # @return [LazyList, Enumerator]
      def each
        return to_enum(:each) unless block_given?

        current = self
        until current.empty?
          yield current.first
          current = current.tail
        end

        self
      end

      # Returns a string representation. Only the already-forced
      # prefix is shown; a trailing `...` marks a list that has
      # not been fully evaluated yet.
      #
      # @return [String]
      def inspect
        values = []
        current = self

        while current.forced? && (current_node = current.node)
          values << current_node.first.inspect
          current = current_node.last
        end

        suffix = current.forced? ? "" : (values.empty? ? "..." : ", ...")
        "LazyList[#{values.join(", ")}#{suffix}]"
      end
      alias_method :to_s, :inspect

      # Returns the LazyList monad.
      #
      # @return [Monad]
      def monad = LazyList

      # Returns self.
      #
      # @return [LazyList]
      def to_monad = self

      protected

      # Forces (once) and memoizes the next node. A node is either
      # `nil` (end of list) or a `[head, tail]` pair.
      #
      # @api private
      def node
        @node = @thunk.call unless forced?
        @forced = true
        @node
      end

      # Whether the next node has been computed already.
      #
      # @api private
      def forced? = @forced

      # Lazy list constructors.
      #
      # @api public
      module Mixin
        # @see Dry::Monads::LazyList
        LazyList = LazyList

        # LazyList constructor.
        # @return [LazyList]
        def LazyList(value) = LazyList.coerce(value)
      end
    end

    require "dry/monads/registry"
    register_mixin(:lazy_list, LazyList::Mixin)
  end
end
