require 'concurrent/constants'

module Concurrent
  class Map
    # A fixed, point-in-time read-only view of the entries of a {Concurrent::Map}.
    #
    # A snapshot captures the key and value object references present when
    # {Concurrent::Map#snapshot} is called. Later insertion, deletion or value
    # replacement on the originating map never changes this view, and iterating
    # the view more than once always yields the same entries.
    #
    # The snapshot is shallow: it retains the very same value objects stored in
    # the map rather than deep-copying them. Mutations performed on a value
    # object's internal state remain visible through the snapshot, exactly as
    # they are through the map itself. Nothing here claims to freeze or protect
    # the internals of the stored values.
    #
    # A snapshot is not itself a {Concurrent::Map} and cannot be mutated; use
    # {#to_h} when a plain, mutable `Hash` of the captured entries is needed.
    class Snapshot
      include ::Enumerable

      # @!visibility private
      # The +entries+ are copied and frozen so that no caller can mutate the
      # pairs backing the view; the captured key and value objects themselves
      # are left untouched (shallow snapshot).
      def initialize(entries)
        @entries = entries.map { |key, value| [key, value] }.freeze
        @hash    = nil
      end

      # Iterates over each captured key/value pair.
      # @yield [key, value]
      # @yieldparam key [Object]
      # @yieldparam value [Object]
      # @return [Enumerator, self] an Enumerator when no block is given, self otherwise
      def each_pair
        return enum_for(:each_pair) unless block_given?

        @entries.each do |key, value|
          yield key, value
        end
        self
      end

      alias each each_pair

      # Iterates over each captured key.
      # @yield [key]
      # @return [Enumerator, self]
      def each_key
        return enum_for(:each_key) unless block_given?

        @entries.each { |key, _value| yield key }
        self
      end

      # Iterates over each captured value.
      # @yield [value]
      # @return [Enumerator, self]
      def each_value
        return enum_for(:each_value) unless block_given?

        @entries.each { |_key, value| yield value }
        self
      end

      # @return [::Array<Object>] the captured keys
      def keys
        @entries.map { |key, _value| key }
      end

      # @return [::Array<Object>] the captured values
      def values
        @entries.map { |_key, value| value }
      end

      # Returns the captured value for +key+, or +nil+ when the key is not part
      # of the snapshot. Note that, like `Hash#[]`, +nil+ does not distinguish a
      # missing key from an explicitly stored +nil+ value; use {#key?} or
      # {#fetch} for that.
      # @param [Object] key
      # @return [Object, nil]
      def [](key)
        to_h[key]
      end

      # Fetch a captured value with +key+, or the given default / block result
      # when the key is not part of the snapshot.
      # @param [Object] key
      # @param [Object] default_value
      # @yield [key] default value for a missing key
      # @return [Object] the value or default value
      # @raise [KeyError] when the key is missing and neither a default value
      #   nor a block is given
      def fetch(key, default_value = NULL)
        if NULL != (value = to_h.fetch(key, NULL))
          value
        elsif block_given?
          yield key
        elsif NULL != default_value
          default_value
        else
          raise KeyError, 'key not found'
        end
      end

      # @param [Object] key
      # @return [true, false] whether +key+ is part of the snapshot
      def key?(key)
        to_h.key?(key)
      end
      alias has_key? key?

      # @param [Object] value
      # @return [true, false] whether +value+ (by `#==`) is captured in the snapshot
      def value?(value)
        @entries.any? { |_key, stored_value| stored_value == value }
      end
      alias has_value? value?

      # @param [Object] value
      # @return [Object, nil] the first captured key whose value equals +value+
      def key(value)
        pair = @entries.find { |_key, stored_value| stored_value == value }
        pair && pair.first
      end

      # @return [Integer] the number of captured entries
      def size
        @entries.size
      end
      alias length size

      # @return [true, false] whether the snapshot captured no entries
      def empty?
        @entries.empty?
      end

      # @return [::Hash] a plain, mutable Hash with the captured entries.
      #   Every call returns a fresh copy that the caller owns and may freely
      #   mutate without affecting this snapshot.
      def to_h
        (@hash ||= @entries.to_h).dup
      end
      alias to_hash to_h

      # @return [::Array<::Array>] the captured `[key, value]` pairs
      def to_a
        @entries.map { |pair| pair.dup }
      end

      # @param [Object] other
      # @return [true, false] whether +other+ is a Snapshot or Hash containing
      #   exactly the captured key/value pairs
      def ==(other)
        case other
        when Snapshot
          @entries == other.send(:entries)
        when ::Hash
          to_h == other
        else
          false
        end
      end

      # @return [String]
      def inspect
        format '#<%s:0x%x %s>', self.class.name, object_id, to_h.inspect
      end
      alias to_s inspect

      # Snapshots are fixed views, mirroring Concurrent::Map which is unfreezable
      # by design; freezing would only give a false sense of value-level safety
      # since the captured value objects themselves are not frozen.
      undef_method :freeze

      private

      attr_reader :entries
    end
  end
end
