# encoding: utf-8
# frozen_string_literal: true

require "amq/bit_set"

module AMQ
  # Simple bitset-based integer allocator, heavily inspired by com.rabbitmq.utility.IntAllocator class
  # in the RabbitMQ Java client.
  #
  # Unlike monotonically incrementing identifier, this allocator is suitable for very long running programs
  # that aggressively allocate and release channels.
  #
  # Two range modes are available:
  #
  #  * the historical mode, `IntAllocator.new(lo, hi)`, keeps the original conventions: capacity is
  #    `hi - lo`, `hi` itself is never handed out and values start at 1
  #  * the closed interval mode, `IntAllocator.inclusive(lo, hi)` (or `new(lo, hi, inclusive: true)`),
  #    hands out every value of `lo..hi`, both boundaries included, so any lower boundary works and
  #    `lo == hi` is a valid single value range
  #
  # Allocation returns a {Reservation}. A reservation compares, hashes, sorts, formats and coerces like
  # the Integer it stands for, so code that treats the result as an integer keeps working. It also records
  # the allocation generation it was issued in, which is what makes a reservation obtained before a
  # {#reset} unable to release an identically numbered reservation of the next generation.
  class IntAllocator

    # Value returned by {#allocate} when the range is exhausted, kept for compatibility with the
    # historical interface.
    EXHAUSTED = -1

    # Integer-compatible handle for one allocated value.
    #
    # `reservation == 3`, `3 == reservation`, `[reservation] == [3]`, `reservation.to_i`, `"#{reservation}"`,
    # arithmetic through `#coerce` and use as a Hash key all behave like the plain Integer. The generation
    # has to travel with the value, therefore `is_a?(Integer)` is false: use {#to_i} or {#id} whenever a
    # real Integer is required.
    class Reservation
      include Comparable

      # @return [Integer] The allocated value
      attr_reader :id
      # @return [Integer] Allocation generation this reservation belongs to
      attr_reader :epoch
      # @return [IntAllocator] Allocator that issued this reservation
      attr_reader :owner

      # @api private
      def initialize(id, epoch, owner)
        @id    = id
        @epoch = epoch
        @owner = owner
        freeze
      end

      def to_i
        @id
      end

      def to_int
        @id
      end

      def ==(other)
        @id == unwrap(other)
      end

      def eql?(other)
        @id.eql?(unwrap(other))
      end

      def hash
        @id.hash
      end

      def <=>(other)
        @id <=> unwrap(other)
      end

      def coerce(other)
        [other, @id]
      end

      def inspect
        "#<#{self.class} #{@id}>"
      end

      def to_s
        @id.to_s
      end

      def method_missing(name, *arguments, &block)
        return super unless @id.respond_to?(name)

        @id.public_send(name, *arguments.map { |argument| unwrap(argument) }, &block)
      end

      def respond_to_missing?(name, include_private = false)
        @id.respond_to?(name, include_private) || super
      end

      private

      def unwrap(other)
        other.is_a?(Reservation) ? other.id : other
      end
    end # Reservation

    #
    # API
    #

    # @return [Integer] Number of integers in the allocation range
    attr_reader :number_of_bits
    # @return [Integer] Upper boundary of the integer range available for allocation
    attr_reader :hi
    # @return [Integer] Lower boundary of the integer range available for allocation
    attr_reader :lo
    # @return [Integer] Current allocation generation, advanced by {#reset}
    attr_reader :epoch

    # Builds an allocator over the closed interval `lo..hi`.
    #
    # @return [IntAllocator]
    def self.inclusive(lo, hi)
      new(lo, hi, inclusive: true)
    end

    # @param [Integer] lo Lower boundary of the integer range available for allocation
    # @param [Integer] hi Upper boundary of the integer range available for allocation
    # @param [Boolean] inclusive When true the range is `lo..hi`, otherwise the historical `[lo, hi)`
    #   capacity convention with values starting at 1 is used
    # @raise [ArgumentError] if the range is empty
    def initialize(lo, hi, inclusive: false)
      raise TypeError, "lower boundary must be an Integer (given: #{lo.inspect})" unless lo.is_a?(Integer)
      raise TypeError, "upper boundary must be an Integer (given: #{hi.inspect})" unless hi.is_a?(Integer)

      @inclusive = !!inclusive
      if @inclusive
        raise ArgumentError, "upper boundary must not be less than the lower one (given: hi = #{hi}, lo = #{lo})" if hi < lo
      else
        raise ArgumentError, "upper boundary must be greater than the lower one (given: hi = #{hi}, lo = #{lo})" unless hi > lo
      end

      @hi = hi
      @lo = lo

      @number_of_bits = @inclusive ? (hi - lo + 1) : (hi - lo)
      @free_set       = BitSet.new(@number_of_bits)

      @allocated_count = 0
      @cursor          = 0
      @epoch           = 0
    end # initialize(lo, hi, inclusive: false)

    # @return [Boolean] true when the allocator uses the closed interval `lo..hi`
    def inclusive?
      @inclusive
    end

    # @return [Integer] Number of integers in the allocation range
    alias capacity number_of_bits

    # @return [Integer] Number of currently allocated integers
    def allocated_count
      @allocated_count
    end

    # @return [Integer] Number of integers available for allocation
    def free_count
      @number_of_bits - @allocated_count
    end

    # Attempts to allocate the next available integer.
    #
    # The search resumes at the position following the previous allocation and {BitSet#next_clear_bit_from}
    # skips whole full words, so a dense allocate/free workload does not rescan the range bit by bit from
    # its beginning on every call.
    #
    # Exhaustion is reported with {#exhaustion_value}: `-1` in the historical mode, as before, and nil in
    # the closed interval mode, where any Integer (including -1) can be a real value of the range.
    #
    # @return [Reservation, Integer, nil] Reservation for the allocated value, or {#exhaustion_value}
    def allocate
      return exhaustion_value if @allocated_count >= @number_of_bits

      index = next_free_index(@cursor)
      return exhaustion_value if index.nil?

      claim(index)
      Reservation.new(value_at(index), @epoch, self)
    end # allocate

    # Value returned by {#allocate} when nothing can be allocated.
    #
    # @return [Integer, nil] `-1` in the historical mode, nil in the closed interval mode
    def exhaustion_value
      EXHAUSTED unless @inclusive
    end

    # Allocates `count` distinct integers in one step.
    #
    # The batch is all or nothing: when fewer than `count` integers are available the bitmap is left
    # untouched and nil is returned. A successful call returns `count` reservations whose values are
    # pairwise distinct and inside the allocation range.
    #
    # @param [Integer] count Number of integers to allocate
    # @return [Array<Reservation>, nil] Reservations, or nil when the remaining capacity is smaller
    # @raise [ArgumentError] if `count` is negative
    def allocate_batch(count)
      raise TypeError, "batch size must be an Integer (given: #{count.inspect})" unless count.is_a?(Integer)
      raise ArgumentError, "batch size must not be negative (given: #{count})" if count < 0

      return [] if count == 0
      return nil if count > free_count

      claimed  = []
      position = @cursor
      wrapped  = false

      while claimed.length < count
        index = position < @number_of_bits ? @free_set.next_clear_bit_from(position) : -1

        if index < 0
          if wrapped
            claimed.each { |claimed_index| @free_set.unset(claimed_index) }
            return nil
          end

          wrapped  = true
          position = 0
          next
        end

        @free_set.set(index)
        claimed << index
        position = index + 1
      end

      @allocated_count += claimed.length
      @cursor = claimed.last + 1

      claimed.map { |index| Reservation.new(value_at(index), @epoch, self) }
    end # allocate_batch
    alias allocate_many allocate_batch
    alias reserve_batch allocate_batch

    # Releases a previously allocated integer.
    #
    # Values that were never allocated, are outside the range, or are already free have no effect on the
    # bitmap. A reservation obtained before the last {#reset} never releases a value of the current
    # generation, even when both carry the same number. A plain Integer is taken as a value of the current
    # generation, which keeps the historical `free(1)` style of calls working.
    #
    # @param [Integer, Reservation, nil] reservation The value to release
    # @return [NilClass] nil
    def free(reservation)
      index = index_of(reservation)
      return nil if index.nil?
      return nil unless @free_set.get(index)

      @free_set.unset(index)
      @allocated_count -= 1
      @cursor = index if index < @cursor
      nil
    end # free(reservation)
    alias release free

    # Releases several reservations, ignoring unknown, out of range and already released values.
    #
    # @param [Array<Integer, Reservation>] reservations Values to release
    # @return [NilClass] nil
    def free_batch(reservations)
      reservations.each { |reservation| free(reservation) }
      nil
    end # free_batch(reservations)
    alias release_batch free_batch

    # @return [Boolean] true if provided argument was previously allocated, false otherwise
    def allocated?(reservation)
      index = index_of(reservation)
      return false if index.nil?

      @free_set.get(index)
    end # allocated?(reservation)

    # Releases the whole allocation range.
    #
    # Reservations issued before this call become stale: they no longer release, nor report themselves as
    # allocated, for the next generation of the same range.
    def reset
      @free_set.clear
      @allocated_count = 0
      @cursor          = 0
      @epoch          += 1
      nil
    end # reset

    private

    # @api private
    def value_at(index)
      @inclusive ? @lo + index : index + 1
    end # value_at(index)

    # Maps a reservation, a stale reservation, an Integer or nil to a bitmap index of the current
    # generation. Returns nil for anything that must not touch the bitmap.
    def index_of(reservation)
      value =
        case reservation
        when nil
          return nil
        when Reservation
          return nil unless reservation.owner.equal?(self)
          return nil unless reservation.epoch == @epoch

          reservation.id
        when Integer
          reservation
        else
          return nil unless reservation.respond_to?(:to_int)

          reservation.to_int
        end

      return nil unless value.is_a?(Integer)

      if @inclusive
        return nil if value < @lo || value > @hi

        value - @lo
      else
        return nil if value < 1 || value > @number_of_bits

        value - 1
      end
    end # index_of(reservation)

    def claim(index)
      @free_set.set(index)
      @allocated_count += 1
      @cursor = index + 1
    end # claim(index)

    # Finds the next free position starting at `start`, wrapping around to the beginning of the range, so
    # that earlier free positions are still reachable after the cursor passed them. Freeing a position
    # moves the cursor back to it, so a dense workload keeps reusing nearby values.
    def next_free_index(start)
      if start >= @number_of_bits
        start = 0
      end

      index = @free_set.next_clear_bit_from(start)
      index = @free_set.next_clear_bit_from(0) if index < 0 && start > 0
      index < 0 ? nil : index
    end # next_free_index(start)

  end # IntAllocator
end # AMQ
