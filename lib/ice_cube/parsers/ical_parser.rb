module IceCube
  class IcalParser
    # RRULE components IceCube knows how to turn into rule behavior.
    KNOWN_RULE_COMPONENTS = %w[
      FREQ INTERVAL COUNT UNTIL WKST
      BYSECOND BYMINUTE BYHOUR BYDAY BYMONTHDAY BYMONTH BYYEARDAY BYSETPOS
    ].freeze

    def self.schedule_from_ical(ical_string, options = {})
      data = {}

      # First join lines that are wrapped
      lines = []
      ical_string.each_line do |line|
        if lines[-1] && line =~ /\A[ \t].+/
          lines[-1] = lines[-1].strip + line.sub(/\A[ \t]+/, "")
        else
          lines << line
        end
      end

      lines.each do |line|
        (property, value) = line.split(":")
        (property, _tzid) = property.split(";")
        case property
        when "DTSTART"
          data[:start_time] = TimeUtil.deserialize_time(value)
        when "DTEND"
          data[:end_time] = TimeUtil.deserialize_time(value)
        when "RDATE"
          data[:rtimes] ||= []
          data[:rtimes] += value.split(",").map { |v| TimeUtil.deserialize_time(v) }
        when "EXDATE"
          data[:extimes] ||= []
          data[:extimes] += value.split(",").map { |v| TimeUtil.deserialize_time(v) }
        when "DURATION"
          data[:duration] # FIXME
        when "RRULE"
          data[:rrules] ||= []
          data[:rrules] += [rule_from_ical(value)]
        end
      end
      Schedule.from_hash data
    end

    def self.rule_from_ical(ical)
      parsed = parse_rule(ical)
      if (extension = parsed.extensions.first)
        raise ArgumentError, "Invalid rule validation type: #{extension.name}"
      end
      parsed.to_rule
    end

    # Parse an RRULE value into a ParsedRule, preserving unrecognized
    # extension components instead of dropping them. Component names are
    # matched case-insensitively; extension values are kept verbatim.
    # Repeating a known (or registered) component raises ArgumentError,
    # while unrecognized extensions may repeat and are all preserved.
    def self.parse_rule(ical)
      raise ArgumentError, "empty ical rule" if ical.nil?

      params = {validations: {}, interval: 1}
      extensions = []
      registered = []
      seen = []

      ical.split(";").each do |component|
        (name, value) = component.split("=", 2)
        raise ArgumentError, "Invalid iCal rule component" if value.nil?
        key = name.strip.upcase
        raise ArgumentError, "Invalid iCal rule component" if key.empty?

        if (handler = extension_handlers[key])
          raise ArgumentError, "Duplicate iCal rule component: #{key}" if seen.include?(key)
          seen << key
          registered << [handler, value]
        elsif KNOWN_RULE_COMPONENTS.include?(key)
          raise ArgumentError, "Duplicate iCal rule component: #{key}" if seen.include?(key)
          seen << key
          apply_rule_component(params, key, value.strip)
        else
          extensions << ParsedRule::Extension.new(key, value)
        end
      end

      rule = Rule.from_hash(params)
      registered.each do |handler, value|
        rule = handler.call(rule, value) || rule
      end
      ParsedRule.new(rule, extensions)
    end

    # Register a handler so a previously unrecognized RRULE component is
    # understood. The handler receives the rule built from the known
    # components and the raw (unstripped) component value; it may mutate
    # the rule or return a replacement.
    def self.register_extension(name, &handler)
      raise ArgumentError, "extension handler block is required" unless handler
      key = normalize_component_name(name)
      if KNOWN_RULE_COMPONENTS.include?(key)
        raise ArgumentError, "Cannot register extension for known RRULE component: #{key}"
      end
      extension_handlers[key] = handler
      key
    end

    def self.unregister_extension(name)
      extension_handlers.delete(normalize_component_name(name))
    end

    def self.extension_registered?(name)
      extension_handlers.key?(normalize_component_name(name))
    end

    def self.extension_handlers
      @extension_handlers ||= {}
    end

    def self.normalize_component_name(name)
      key = name.to_s.strip.upcase
      raise ArgumentError, "Invalid extension name" if key.empty?
      key
    end
    private_class_method :normalize_component_name

    def self.apply_rule_component(params, name, value)
      validations = params[:validations]
      case name
      when "FREQ"
        params[:rule_type] = "IceCube::#{value[0].upcase}#{value.downcase[1..]}Rule"
      when "INTERVAL"
        params[:interval] = value.to_i
      when "COUNT"
        params[:count] = value.to_i
      when "UNTIL"
        params[:until] = TimeUtil.deserialize_time(value).utc
      when "WKST"
        params[:week_start] = TimeUtil.ical_day_to_symbol(value.upcase)
      when "BYSECOND"
        validations[:second_of_minute] = value.split(",").map(&:to_i)
      when "BYMINUTE"
        validations[:minute_of_hour] = value.split(",").map(&:to_i)
      when "BYHOUR"
        validations[:hour_of_day] = value.split(",").map(&:to_i)
      when "BYDAY"
        dows = {}
        days = []
        value.split(",").each do |expr|
          expr = expr.strip.upcase
          day = TimeUtil.ical_day_to_symbol(expr[-2..])
          if expr.length > 2 # day with occurence
            occ = expr[0..-3].to_i
            dows[day].nil? ? dows[day] = [occ] : dows[day].push(occ)
            days.delete(TimeUtil.sym_to_wday(day))
          elsif dows[day].nil?
            days.push TimeUtil.sym_to_wday(day)
          end
        end
        validations[:day_of_week] = dows unless dows.empty?
        validations[:day] = days unless days.empty?
      when "BYMONTHDAY"
        validations[:day_of_month] = value.split(",").map(&:to_i)
      when "BYMONTH"
        validations[:month_of_year] = value.split(",").map(&:to_i)
      when "BYYEARDAY"
        validations[:day_of_year] = value.split(",").map(&:to_i)
      when "BYSETPOS"
        validations[:by_set_pos] = value.split(",").map(&:to_i)
      end
    end
    private_class_method :apply_rule_component
  end
end
