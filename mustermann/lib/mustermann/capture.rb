# frozen_string_literal: true

module Mustermann
  # Describes where and how a single capture group participated in a match.
  #
  # Unlike the values in {Mustermann::Match#params}, offsets are never computed
  # by searching for a decoded value inside the original string: they come
  # straight from the regular expression engine (or the equivalent trie bookkeeping).
  #
  # @example
  #   pattern = Mustermann.new('/users/:id')
  #   match   = pattern.match('/users/42')
  #   capture = match.capture_occurrences.first
  #   capture.name        # => "id"
  #   capture.span        # => [7, 9]
  #   capture.bytes       # => "42"
  #   match.string.byteslice(capture.start, capture.length) # => "42"
  class Capture
    # @return [String] name of the capture group (as used in {Mustermann::Match#params})
    attr_reader :name

    # @return [Integer, nil] byte offset of the first captured byte in the
    #   originally matched string (inclusive), or nil if the capture did not
    #   participate (e.g. an optional segment that was not present)
    attr_reader :start

    # @return [Integer, nil] byte offset directly after the last captured byte
    #   in the originally matched string (exclusive), or nil if the capture did
    #   not participate
    attr_reader :stop

    # @param name  [String] capture group name
    # @param start [Integer, nil] inclusive start byte offset (nil for absent captures)
    # @param stop  [Integer, nil] exclusive end byte offset (nil for absent captures)
    # @param string [String] the originally matched string
    def initialize(name, start, stop, string = nil)
      @name   = name
      @start  = start
      @stop   = stop
      @string = string
      freeze
    end

    # @return [Range<Integer>, nil] half-open byte range, or nil if the capture
    #   did not participate in the match
    def range
      start...stop if present?
    end

    # @return [Array(Integer, Integer)] half-open byte range as a two element
    #   array; both entries are nil for captures that did not participate
    def span
      [start, stop]
    end

    # @return [String, nil] the raw, URI-encoded text captured from the original
    #   input. This is the empty string when a capture genuinely matched an
    #   empty substring (e.g. an empty splat), and nil when an optional segment
    #   was not present.
    def bytes
      return unless present?
      @string.byteslice(start, length)
    end

    alias raw bytes

    # @return [Boolean] whether the capture group participated in the match at
    #   all. A capture that matched the empty string is still present.
    def present?
      !start.nil?
    end

    # @return [Boolean] whether the capture participated but consumed zero bytes
    def empty?
      present? && start == stop
    end

    # @return [Integer] number of bytes captured (zero for empty but present captures)
    def length
      present? ? stop - start : 0
    end

    alias size length

    # @return [String]
    def inspect
      "#<#{self.class.name} #{name.inspect} #{span.inspect} #{bytes.inspect}>"
    end

    # @see Object#==
    def ==(other)
      other.is_a?(self.class) &&
        name == other.name && span == other.span && bytes == other.bytes
    end

    alias eql? ==

    # @see Object#hash
    def hash
      [self.class, name, start, stop, bytes].hash
    end

    # Returns a copy of the capture with offsets shifted and text taken from
    # another string. Used when stitching matches of substrings together.
    # @!visibility private
    def remap(string, offset)
      return self.class.new(name, nil, nil, string) unless present?
      self.class.new(name, start + offset, stop + offset, string)
    end
  end
end
