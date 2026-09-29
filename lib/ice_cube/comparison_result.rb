module IceCube
  # Outcome of comparing the actual occurrences produced by two schedules
  # over a bounded time interval. See Schedule#compare_occurrences.
  #
  # The comparison is performed against the times that really occur, never
  # against serialized representations.
  class ComparisonResult
    # Either +:equal+ (no difference found inside the interval),
    # +:different+ (a difference was found) or +:incomplete+ (the work limit
    # was reached before the question could be answered).
    attr_reader :status

    # The earliest time that was produced by only one side. Set only when
    # the status is +:different+. The returned object is an Occurrence.
    attr_reader :time

    # Which side produced +time+ exclusively: +:self+ or +:other+.
    attr_reader :side

    # Number of matched occurrence pairs consumed while walking the streams.
    attr_reader :examined

    # The point up to which both schedules have been verified to agree. Set
    # only when the status is +:incomplete+; resume the comparison by passing
    # it as the next interval's beginning time.
    attr_reader :verified_through

    def initialize(status, time: nil, side: nil, examined: 0, verified_through: nil)
      @status = status
      @time = time
      @side = side
      @examined = examined
      @verified_through = verified_through
    end

    def equal?
      status == :equal
    end

    def different?
      status == :different
    end

    def incomplete?
      status == :incomplete
    end

    # True when the earliest differing time came from the receiver.
    def only_in_self?
      side == :self
    end

    # True when the earliest differing time came from the other schedule.
    def only_in_other?
      side == :other
    end

    def ==(other)
      other.is_a?(ComparisonResult) &&
        status == other.status &&
        time == other.time &&
        side == other.side
    end
    alias_method :eql?, :==

    def to_a
      [status, time, side]
    end
  end
end
