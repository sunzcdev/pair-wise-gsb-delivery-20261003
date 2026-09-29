module IceCube
  class IcalParser
    # RRULE fields IceCube knows how to turn into an executable rule.
    KNOWN_RULE_FIELDS = %w[
      FREQ INTERVAL COUNT UNTIL WKST
      BYSECOND BYMINUTE BYHOUR BYDAY BYMONTHDAY BYMONTH BYYEARDAY BYSETPOS
    ].freeze

    # The result of parsing a single RRULE: the executable rule built from
    # the fields IceCube understands, kept separate from any extension
    # fields it does not.
    class ParsedRule
      # The executable IceCube::Rule built from the known fields.
      attr_reader :rule

      # Extension fields, as an ordered hash of uppercase field name to the
      # raw (unparsed) string value.
      attr_reader :extensions

      def initialize(rule, extensions)
        @rule = rule
        @extensions = extensions
      end

      def extension_fields?
        !extensions.empty?
      end
    end

    def self.schedule_from_ical(ical_string, options = {})
      data = {}
      extensions_mode = options.fetch(:extensions, :reject)

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
          data[:rrules] += [rule_from_ical(value, extensions: extensions_mode)]
        end
      end
      Schedule.from_hash data
    end

    # Register a handler so an otherwise-unknown RRULE field is recognized.
    # The handler receives the parsed IceCube::Rule and the raw string value.
    def self.register_extension(name, &handler)
      raise ArgumentError, "extension handler block required" unless handler
      extension_handlers[normalize_field_name(name)] = handler
    end

    def self.unregister_extension(name)
      extension_handlers.delete(normalize_field_name(name))
    end

    def self.extension_registered?(name)
      extension_handlers.key?(normalize_field_name(name))
    end

    def self.extension_handlers
      @extension_handlers ||= {}
    end

    # Parse an RRULE string, keeping the executable rule and any
    # unrecognized extension fields separate. Field names are
    # case-insensitive and stored uppercase; values are kept as raw
    # strings. Repeating any field raises ArgumentError.
    def self.parse_rule(ical)
      raise ArgumentError, "empty ical rule" if ical.nil?

      validations = {}
      params = {validations: validations, interval: 1}
      extensions = {}
      registered = []
      seen = {}

      ical.split(";").each do |component|
        (name, value) = component.split("=", 2)
        raise ArgumentError, "Invalid iCal rule component" if value.nil?
        name = normalize_field_name(name)
        value = value.strip
        raise ArgumentError, "Duplicate iCal rule field: #{name}" if seen.key?(name)
        seen[name] = true

        if extension_registered?(name)
          registered << [name, value]
          next
        end

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
        else
          extensions[name] = value
        end
      end

      rule = Rule.from_hash(params)
      registered.each do |name, value|
        extension_handlers[name].call(rule, value)
      end
      ParsedRule.new(rule, extensions)
    end

    # Parse an RRULE string into an executable rule. Unknown extension
    # fields are never silently dropped: callers must explicitly choose
    #   extensions: :reject   (default) raise UnknownIcalFieldsError
    #   extensions: :ignore   discard the unknown fields
    #   extensions: :preserve keep them on the rule for re-serialization
    def self.rule_from_ical(ical, extensions: :reject)
      parsed = parse_rule(ical)

      case extensions
      when :reject
        if parsed.extension_fields?
          raise UnknownIcalFieldsError, parsed.extensions.keys
        end
      when :ignore
        # Caller has explicitly chosen to discard the extension fields.
      when :preserve
        parsed.rule.ical_extensions = parsed.extensions
      else
        raise ArgumentError, "Invalid extensions mode: #{extensions.inspect}. " \
                             "Expected :reject, :ignore or :preserve"
      end

      parsed.rule
    end

    def self.normalize_field_name(name)
      name.strip.upcase
    end
    private_class_method :normalize_field_name
  end
end
