# encoding: utf-8
# frozen_string_literal: true

module AMQ
  # Very minimalistic, pure Ruby implementation of bit set. Inspired by java.util.BitSet,
  # although significantly smaller in scope.
  #
  # Originally part of amqp gem. Extracted to make it possible for Bunny to use it.
  class BitSet

    attr_reader :words_in_use
    #
    # API
    #

    ADDRESS_BITS_PER_WORD = 6
    BITS_PER_WORD         = (1 << ADDRESS_BITS_PER_WORD)
    WORD_MASK             = 0xffffffffffffffff

    # @param [Integer] Number of bits in the set
    # @api public
    def initialize(nbits)
      @nbits = nbits

      self.init_words(nbits)
    end # initialize(nbits)

    # Sets (flags) given bit. This method allows bits to be set more than once in a row, no exception will be raised.
    #
    # @param [Integer] A bit to set
    # @api public
    def set(i)
      check_range(i)
      w = self.word_index(i)
      result = @words[w] |= (1 << (i % BITS_PER_WORD))
      result
    end # set(i)

    # Fetches flag value for given bit.
    #
    # @param [Integer] A bit to fetch
    # @return [Boolean] true if given bit is set, false otherwise
    # @api public
    def get(i)
      check_range(i)
      w = self.word_index(i)

      (@words[w] & (1 << i % BITS_PER_WORD)) != 0
    end # get(i)
    alias [] get

    # Unsets (unflags) given bit. This method allows bits to be unset more than once in a row, no exception will be raised.
    #
    # @param [Integer] A bit to unset
    # @api public
    def unset(i)
      check_range(i)
      w = self.word_index(i)
      return if w.nil?

      result = @words[w] &= ~(1 << i % BITS_PER_WORD)
      result
    end # unset(i)

    # Clears all bits in the set
    # @api public
    def clear
      self.init_words(@nbits)
    end # clear

    def next_clear_bit()
      @words.each_with_index do |word, i|
        if word == WORD_MASK
          next
        end
        return i * BITS_PER_WORD + BitSet.number_of_trailing_ones(word)
      end
      -1
    end # next_clear_bit

    # Same as #next_clear_bit, but looks for the first clear bit at or after
    # the given position only. Whole words that are full are skipped, so
    # repeated calls from a moving position do not rescan the beginning.
    #
    # @param [Integer] Position to start the search at, may be equal to the number of bits
    # @return [Integer] Index of the first clear bit at or after the position, -1 if there is none
    # @api public
    def next_clear_bit_from(start)
      unless start.is_a?(Integer) && start >= 0 && start <= @nbits
        raise IndexError, "Cannot start at bit #{start.inspect} from a BitSet with #{@nbits} bits"
      end
      return -1 if start == @nbits

      w    = word_index(start)
      word = @words[w] | ((1 << (start % BITS_PER_WORD)) - 1)

      while true
        unless word == WORD_MASK
          i = w * BITS_PER_WORD + BitSet.number_of_trailing_ones(word)
          return i < @nbits ? i : -1
        end

        w += 1
        return -1 if w >= @words.size
        word = @words[w]
      end
    end # next_clear_bit_from

    def to_s
      result = ""
      @words.each do |w|
        result += w.to_s(2).rjust(BITS_PER_WORD,'0') + ":"
      end
      result
    end # to_s

    #
    # Implementation
    #

    # @private
    def self.number_of_trailing_ones(num)
      0.upto(BITS_PER_WORD) do |bit|
        return bit if num[bit] == 0
      end
      BITS_PER_WORD
    end # number_of_trailing_ones

    # @private
    def word_index(i)
      i >> ADDRESS_BITS_PER_WORD
    end # word_index

    protected

    # @private
    def init_words(nbits)
      n      = word_index(nbits-1) + 1
      @words = Array.new(n) { 0 }
    end # init_words

    def check_range(i)
      if i < 0 || i >= @nbits
        raise IndexError, "Cannot access bit #{i} from a BitSet with #{@nbits} bits"
      end
    end # check_range
  end # BitSet
end # AMQ
