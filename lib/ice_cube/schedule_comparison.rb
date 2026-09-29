module IceCube
  # Compares the occurrences of two schedules within a finite window,
  # reporting the earliest time at which they disagree (and which side
  # it belongs to), or confirming that both schedules occur at the same
  # times throughout the window.
  #
  # The comparison walks the actual occurrences of both schedules, so
  # schedules that are written differently but occur at the same times
  # compare as equal, and serialized representations are never consulted.
  #
  # Because rules may be extremely sparse within the window, the walk is
  # bounded by a work budget (+max_steps+). If the budget is exhausted
  # before a conclusion is reached, the result is +:incomplete+ rather
  # than a guess based on a partial prefix.
  class ScheduleComparison
    # Default cap on the total number of occurrences examined (both
    # sides combined) before giving up with an +:incomplete+ result.
    DEFAULT_MAX_STEPS = 10_000

    # One of :equal, :different or :incomplete
    attr_reader :status

    # The earliest occurrence present on only one side (when :different)
    attr_reader :time

    # Which schedule owns the differing occurrence: :left or :right
    attr_reader :side

    def initialize(status, time = nil, side = nil)
      @status = status
      @time = time
      @side = side
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

    # Compare the occurrence times of +left+ and +right+ between
    # +opening_time+ and +closing_time+ (inclusive), examining at most
    # +max_steps+ occurrences in total before giving up.
    def self.compare(left, right, opening_time, closing_time, max_steps: DEFAULT_MAX_STEPS)
      raise ArgumentError, "max_steps must be positive" unless max_steps.is_a?(Integer) && max_steps > 0

      enums = [left, right].map { |s| window_enumerator(s, opening_time, closing_time) }
      current = [nil, nil]
      stale = [true, true]
      steps = 0

      loop do
        2.times do |i|
          next unless stale[i]
          return new(:incomplete) if steps >= max_steps
          steps += 1
          begin
            current[i] = enums[i].next
          rescue StopIteration
            current[i] = nil
          end
          stale[i] = false
        end

        a, b = current
        return new(:equal) if a.nil? && b.nil?
        return new(:different, b, :right) if a.nil?
        return new(:different, a, :left) if b.nil?

        a_time = a.start_time
        b_time = b.start_time
        if a_time == b_time
          stale = [true, true]
        elsif a_time < b_time
          return new(:different, a, :left)
        else
          return new(:different, b, :right)
        end
      end
    end

    # Lazily enumerate a schedule's occurrences within the window,
    # stopping as soon as an occurrence falls past +closing_time+.
    def self.window_enumerator(schedule, opening_time, closing_time)
      enum = schedule.remaining_occurrences_enumerator(opening_time)
      Enumerator.new do |yielder|
        loop do
          occurrence = enum.next
          break if occurrence.start_time > closing_time
          yielder << occurrence
        end
      end
    end
    private_class_method :window_enumerator
  end
end
