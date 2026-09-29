module IceCube
  # The result of IcalParser.parse_rule: an executable Rule built from the
  # recognized RRULE components, plus any unrecognized extension components
  # preserved verbatim so they can be inspected or forwarded.
  class ParsedRule
    # A single unrecognized RRULE component. Names are normalized to
    # uppercase (component names are case-insensitive per RFC 5545), while
    # values are kept exactly as received.
    class Extension
      attr_reader :name, :value

      def initialize(name, value)
        @name = name
        @value = value
      end

      def to_s
        "#{name}=#{value}"
      end

      def ==(other)
        other.is_a?(Extension) && name == other.name && value == other.value
      end
      alias_method :eql?, :==

      def hash
        [name, value].hash
      end
    end

    attr_reader :extensions

    def initialize(rule, extensions)
      @rule = rule
      @extensions = extensions.freeze
    end

    def extensions?
      !extensions.empty?
    end

    # Unrecognized extensions are never treated as no-ops: callers must
    # explicitly pass extensions: :ignore to execute the rule anyway.
    # The default (:reject) raises UnknownRuleExtension.
    def to_rule(extensions: :reject)
      case extensions
      when :reject
        if extensions?
          names = self.extensions.map(&:name).join(", ")
          raise UnknownRuleExtension, "Unrecognized RRULE component(s): #{names}. " \
            "Pass extensions: :ignore to execute the rule without them."
        end
      when :ignore
        # caller accepted responsibility for the dropped components
      else
        raise ArgumentError, "extensions must be :reject or :ignore"
      end
      @rule
    end

    # Serialize back to an RRULE value, re-emitting preserved extensions
    # after the recognized components.
    def to_ical
      ([@rule.to_ical] + extensions.map(&:to_s)).reject(&:empty?).join(";")
    end

    def ==(other)
      other.is_a?(ParsedRule) &&
        to_rule(extensions: :ignore) == other.to_rule(extensions: :ignore) &&
        extensions == other.extensions
    end
  end
end
