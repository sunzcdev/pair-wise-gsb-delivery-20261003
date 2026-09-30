# frozen_string_literal: true

module Dry
  module Monads
    # A lazy list monad with fair bind and apply.
    class LazyList
      class << self
        # Builds a lazy list from finite values.
        #
        # @param values [Array<Object>] List elements
        # @return [LazyList]
        def [](*values) = new(values)

        # Coerces a value to a lazy list.
        #
        # @param value [LazyList, Array, Enumerator, Enumerable, nil] Value
        # @return [LazyList]
        def coerce(value)
          case value
          when LazyList
            value
          when nil
            new([])
          when Enumerator, Enumerable
            new(value)
          else
            raise TypeError, "Can't coerce #{value.inspect} to LazyList"
          end
        end

        # Wraps a value with a lazy list.
        #
        # @param value [Object] Any object
        # @return [LazyList]
        def pure(value = Undefined, &block)
          new([Undefined.default(value, block)])
        end
      end

      include Enumerable
      include Transformer

      # @api private
      def initialize(source)
        @source = MemoizedEnumerator.wrap(source)
      end

      # Lifts a block/proc and fairly enumerates the resulting lists.
      #
      # @param args [Array<Object>] Arguments passed to the block or proc
      # @return [LazyList]
      def bind(*args, &block)
        block ||= args.shift
        self.class.new(fair_bind_enumerator(block, args))
      end

      # Maps a block over the list.
      #
      # @param args [Array<Object>] Arguments passed to the block or proc
      # @return [LazyList]
      def fmap(*args, &block)
        block ||= args.shift
        bind { |value| [block.(value, *args)] }
      end

      # Maps a block over the list, or returns an Enumerator.
      #
      # @return [LazyList, Enumerator]
      def map(&block)
        block_given? ? fmap(block) : each
      end

      # Filters elements with a block.
      #
      # @return [LazyList]
      def filter(&block)
        self.class.new(
          Enumerator.new do |yielder|
            each { |value| yielder << value if block.(value) }
          end
        )
      end
      alias_method :select, :filter

      # Applies the stored functions to the elements of the given list.
      #
      # @param list [LazyList, Array, Enumerator, Enumerable]
      # @return [LazyList]
      def apply(list = Undefined, &block)
        values = Undefined.default(list, &block)

        fmap(Curry).bind do |function|
          self.class.coerce(values).fmap { |value| function.(value) }
        end
      end

      # Concatenates two lazy lists.
      #
      # @param other [LazyList, Array, Enumerator, Enumerable]
      # @return [LazyList]
      def +(other)
        other_list = self.class.coerce(other)

        self.class.new(
          Enumerator.new do |yielder|
            each { |value| yielder << value }
            other_list.each { |value| yielder << value }
          end
        )
      end

      # Returns whether the list is empty.
      #
      # @return [Boolean]
      def empty?
        each.next
        false
      rescue StopIteration
        true
      end

      # Enumerates values lazily.
      #
      # @return [Enumerator, self]
      def each(&block)
        return source.enum_for_each unless block

        index = 0

        loop do
          yield source.at(index)
          index += 1
        end

        self
      end

      # Forces enough values and coerces to an array.
      #
      # @return [Array<Object>]
      def to_a = each.to_a
      alias_method :to_ary, :to_a

      # Returns a string representation without consuming values.
      #
      # @return [String]
      def inspect = "LazyList(?)"
      alias_method :to_s, :inspect

      # Returns the LazyList monad.
      #
      # @return [Class]
      def monad = LazyList

      # Returns self.
      #
      # @return [LazyList]
      def to_monad = self

      private

      attr_reader :source

      def fair_bind_enumerator(block, args)
        Enumerator.new do |yielder|
          rows = []
          outer = each

          loop do
            value = begin
              outer.next
            rescue StopIteration
              EXHAUSTED
            end

            break if value.equal?(EXHAUSTED)

            rows << self.class.coerce(block.(value, *args)).each

            advance_rows(rows, yielder)
          end

          advance_rows(rows, yielder) until rows.empty?
        end
      end

      def advance_rows(rows, yielder)
        index = 0

        while index < rows.size
          begin
            yielder << rows[index].next
          rescue StopIteration
            rows.delete_at(index)
          else
            index += 1
          end
        end
      end

      EXHAUSTED = Object.new.freeze

      # Replays a source for every consumer while reading it only once.
      class MemoizedEnumerator
        MEMO_VARIABLE = :@__dry_monads_lazy_list_memo__

        def self.wrap(source)
          return source if source.is_a?(self)

          if source.is_a?(Enumerator)
            source.instance_variable_get(MEMO_VARIABLE) ||
              source.instance_variable_set(MEMO_VARIABLE, new(source))
          else
            new(source)
          end
        rescue FrozenError
          new(source)
        end

        def initialize(source)
          @enumerator = source_for(source)
          @values = []
          @terminal_error = nil
          @exhausted = false
        end

        def at(index)
          until exhausted? || values.size > index
            begin
              values << enumerator.next
            rescue StopIteration
              @exhausted = true
            rescue StandardError => error
              @terminal_error = error
              @exhausted = true
            end
          end

          if index >= values.size
            raise @terminal_error if @terminal_error

            raise StopIteration, "iteration reached an end"
          end

          values.fetch(index)
        end

        def enum_for_each
          index = 0

          Enumerator.new do |yielder|
            loop do
              yielder << at(index)
              index += 1
            end
          end
        end

        private

        attr_reader :enumerator, :values

        def source_for(source)
          source.is_a?(Enumerator) ? source : source.each
        end

        def exhausted? = @exhausted
      end

      # Lazy list constructors.
      module Mixin
        # @see Dry::Monads::LazyList
        LazyList = LazyList

        # @see Dry::Monads::LazyList
        L = LazyList

        # Lazy list constructor.
        # @return [LazyList]
        def LazyList(value) = LazyList.coerce(value)
      end
    end

    require "dry/monads/registry"
    register_mixin(:lazy_list, LazyList::Mixin)
  end
end
