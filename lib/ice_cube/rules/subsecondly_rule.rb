module IceCube
  class SubsecondlyRule < ValidatedRule
    include Validations::SubsecondlyInterval

    def initialize(interval)
      super()
      interval(interval)
      reset
    end

    # Enumeration advances past each occurrence in whole-nanosecond steps so
    # that no occurrence can be skipped; the interval validation then shifts
    # forward to the next exact grid point.
    def min_step
      Validations::SubsecondlyInterval::NANOSECOND
    end

    def subsecond_precision?
      true
    end
  end
end
