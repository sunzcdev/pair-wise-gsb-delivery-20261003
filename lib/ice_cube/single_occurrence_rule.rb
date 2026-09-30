module IceCube
  class SingleOccurrenceRule < Rule
    attr_reader :time

    def initialize(time)
      @time = TimeUtil.ensure_time time
    end

    # Always terminating
    def terminating?
      true
    end

    def next_time(t, _, closing_time, increment: true, skip_until: nil)
      unless closing_time && closing_time < t
        candidate = time if time.to_i >= t.to_i
        candidate if candidate && (skip_until.nil? || candidate > skip_until)
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
