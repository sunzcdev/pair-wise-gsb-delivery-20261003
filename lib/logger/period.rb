# frozen_string_literal: true

class Logger
  module Period
    module_function

    SiD = 24 * 60 * 60

    # When +zone+ is given, calendar boundaries are computed in that
    # explicit, fixed offset (anything accepted by Time#getlocal and
    # Time.new: a String such as "+09:00" or "UTC", or an Integer
    # number of seconds), independent of the host's local time zone
    # configuration.  A fixed offset observes no DST, so the boundary
    # rule itself never hits repeated or missing local times.  The
    # returned Time carries the zone offset, so both the rotation
    # instant and any +strftime+-computed filename suffix derived from
    # it follow the same calendar.
    #
    # When +zone+ is nil, the host's local time zone is used (legacy
    # behavior).
    def next_rotate_time(now, shift_age, zone = nil)
      if zone
        now = now.getlocal(zone)
        case shift_age
        when 'daily', :daily
          t = Time.new(now.year, now.month, now.mday, 0, 0, 0, zone) + SiD
        when 'weekly', :weekly
          t = Time.new(now.year, now.month, now.mday, 0, 0, 0, zone) + SiD * (7 - now.wday)
        when 'monthly', :monthly
          t = Time.new(now.year, now.month, 1, 0, 0, 0, zone) + SiD * 32
          return Time.new(t.year, t.month, 1, 0, 0, 0, zone)
        when 'now', 'everytime', :now, :everytime
          return now
        else
          raise ArgumentError, "invalid :shift_age #{shift_age.inspect}, should be daily, weekly, monthly, or everytime"
        end
        return t
      end
      case shift_age
      when 'daily', :daily
        t = Time.mktime(now.year, now.month, now.mday) + SiD
      when 'weekly', :weekly
        t = Time.mktime(now.year, now.month, now.mday) + SiD * (7 - now.wday)
      when 'monthly', :monthly
        t = Time.mktime(now.year, now.month, 1) + SiD * 32
        return Time.mktime(t.year, t.month, 1)
      when 'now', 'everytime', :now, :everytime
        return now
      else
        raise ArgumentError, "invalid :shift_age #{shift_age.inspect}, should be daily, weekly, monthly, or everytime"
      end
      if t.hour.nonzero? or t.min.nonzero? or t.sec.nonzero?
        hour = t.hour
        t = Time.mktime(t.year, t.month, t.mday)
        t += SiD if hour > 12
      end
      t
    end

    def previous_period_end(now, shift_age, zone = nil)
      if zone
        now = now.getlocal(zone)
        case shift_age
        when 'daily', :daily
          t = Time.new(now.year, now.month, now.mday, 0, 0, 0, zone) - 1
        when 'weekly', :weekly
          t = Time.new(now.year, now.month, now.mday, 0, 0, 0, zone) - SiD * now.wday - 1
        when 'monthly', :monthly
          t = Time.new(now.year, now.month, 1, 0, 0, 0, zone) - 1
        when 'now', 'everytime', :now, :everytime
          return now
        else
          raise ArgumentError, "invalid :shift_age #{shift_age.inspect}, should be daily, weekly, monthly, or everytime"
        end
        return t
      end
      case shift_age
      when 'daily', :daily
        t = Time.mktime(now.year, now.month, now.mday) - SiD / 2
      when 'weekly', :weekly
        t = Time.mktime(now.year, now.month, now.mday) - (SiD * now.wday + SiD / 2)
      when 'monthly', :monthly
        t = Time.mktime(now.year, now.month, 1) - SiD / 2
      when 'now', 'everytime', :now, :everytime
        return now
      else
        raise ArgumentError, "invalid :shift_age #{shift_age.inspect}, should be daily, weekly, monthly, or everytime"
      end
      Time.mktime(t.year, t.month, t.mday, 23, 59, 59)
    end
  end
end
