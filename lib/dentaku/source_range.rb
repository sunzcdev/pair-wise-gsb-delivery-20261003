module Dentaku
  # A half-open character range [start_offset, end_offset) into the
  # original, user-supplied expression string. Offsets are counted in
  # characters (not bytes), so multibyte text such as Chinese identifiers
  # does not shift them.
  class SourceRange
    attr_reader :start_offset, :end_offset

    def initialize(start_offset, end_offset)
      @start_offset = start_offset
      @end_offset   = end_offset
    end

    def length
      end_offset - start_offset
    end

    def empty?
      length.zero?
    end

    def to_range
      (start_offset...end_offset)
    end

    # the exact source text this range points at
    def extract(source)
      source[to_range]
    end

    def ==(other)
      other.is_a?(SourceRange) &&
        start_offset == other.start_offset &&
        end_offset == other.end_offset
    end
    alias_method :eql?, :==

    def hash
      [start_offset, end_offset].hash
    end

    def to_s
      "#{start_offset}...#{end_offset}"
    end

    def inspect
      "#<Dentaku::SourceRange #{self}>"
    end
  end
end
