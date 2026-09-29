module IceCube
  class SingleOccurrenceRule < Rule
    attr_reader :time

    def initialize(time)
      @time = TimeUtil.ensure_time time
      @emitted = false
    end

    # Always terminating
    def terminating?
      true
    end

    # Reset emission state for another enumeration run
    def reset
      @emitted = false
    end

    # Called by Schedule when this rule's occurrence is emitted. A single
    # occurrence can only be emitted once per enumeration, which also keeps
    # the whole-second leniency in next_time from emitting it twice when a
    # schedule steps in sub-second increments.
    def increment_uses
      @emitted = true
    end

    def next_time(t, _, closing_time, increment: true)
      unless closing_time && closing_time < t
        time if !@emitted && time.to_i >= t.to_i
      end
    end

    def to_hash
      {time: time}
    end

    def full_required?
      false
    end
  end
end
