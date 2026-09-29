module IceCube
  # The result of applying a Schedule's BusinessDayAdjustment to the raw
  # occurrence stream: the adjusted occurrence itself plus the original
  # (unadjusted) occurrence start times that were merged into it.
  class AdjustedOccurrence
    include Comparable

    # The adjusted IceCube::Occurrence
    attr_reader :occurrence

    # Original (unadjusted) start times of every raw occurrence that moved
    # to this occurrence's start time, in chronological order
    attr_reader :sources

    def initialize(occurrence, sources)
      @occurrence = occurrence
      @sources = sources
    end

    def start_time
      occurrence.start_time
    end

    def end_time
      occurrence.end_time
    end

    # True when more than one raw occurrence moved to this time
    def merged?
      sources.length > 1
    end

    def <=>(other)
      start_time <=> other.start_time
    end

    def merge!(other)
      sources.concat(other.sources)
      self
    end
  end
end
