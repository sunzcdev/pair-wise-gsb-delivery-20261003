require "digest"
require "json"
require "time"

module IceCube
  class OccurrenceIterator < Enumerator
    CHECKPOINT_VERSION = 1

    def self.resume(schedule, checkpoint, **options)
      new(schedule, nil, nil, checkpoint: checkpoint, **options)
    end

    def initialize(schedule, opening_time = nil, closing_time = nil, checkpoint: nil, spans: false)
      @schedule = schedule
      if checkpoint
        @state = load_checkpoint(checkpoint)
        checkpoint_opening_time = deserialize_time(@state["opening_time"])
        checkpoint_closing_time = deserialize_time(@state["closing_time"])
        requested_opening_time = opening_time.nil? ? nil : normalize_opening_time(opening_time)
        requested_closing_time = TimeUtil.match_zone(closing_time, schedule.start_time)
        @spans = @state["spans"] == true
        verify_boundaries(@state, spans)
        verify_requested_boundary(:opening_time, requested_opening_time, checkpoint_opening_time)
        verify_requested_boundary(:closing_time, requested_closing_time, checkpoint_closing_time)
        @opening_time = checkpoint_opening_time
        @closing_time = checkpoint_closing_time
        verify_schedule_fingerprint_value(@state["schedule_fingerprint"])
      else
        @spans = spans == true && schedule.duration != 0
        @opening_time = normalize_opening_time(opening_time || schedule.start_time)
        @closing_time = TimeUtil.match_zone(closing_time, schedule.start_time)
        @state = initial_state(schedule_fingerprint)
      end

      super() do |yielder|
        each_occurrence(yielder)
      end
    end

    def checkpoint
      @state = state_with_fingerprint(current_state)
      deep_dup(@state)
    end
    alias_method :to_checkpoint, :checkpoint

    def checkpoint_json
      JSON.generate(checkpoint)
    end
    alias_method :to_checkpoint_json, :checkpoint_json

    private

    def each_occurrence(yielder)
      expected_fingerprint = schedule_fingerprint
      verify_schedule_fingerprint(expected_fingerprint)

      reset_rules
      resume_from = last_occurrence_time
      restore_rules if resume_from

      after_time = resume_from
      search_time = initial_search_time(resume_from)

      loop do
        occurrence = @schedule.send(
          :next_time,
          search_time,
          @closing_time,
          after: after_time
        )
        break unless occurrence
        break if @closing_time && occurrence > @closing_time

        if visible_occurrence?(occurrence)
          @state = state_with_fingerprint(state_for(occurrence.start_time, expected_fingerprint))
          yielder << occurrence
        end

        search_time = occurrence.start_time + 1
        after_time = nil
        resume_from = nil
      end
    end

    def initial_search_time(resume_from)
      if resume_from
        resume_from
      else
        time = @schedule.send(:full_required?) ? @schedule.start_time : @opening_time
        time -= @schedule.duration if @spans
        time < @schedule.start_time ? @schedule.start_time : time
      end
    end

    def visible_occurrence?(occurrence)
      if @spans
        occurrence.end_time > @opening_time
      else
        occurrence >= @opening_time
      end
    end

    def reset_rules
      @schedule.send(:reset)
    end

    def restore_rules
      search_time = last_occurrence_time
      restore_rule_group(@state["recurrence_rules"], recurrence_rule_contexts, search_time)
      restore_rule_group(@state["exception_rules"], exception_rule_contexts, search_time)
    end

    def restore_rule_group(state_rules, rules, search_time)
      unless state_rules.length == rules.length
        raise ArgumentError, "IceCube checkpoint does not match the current schedule"
      end

      state_rules.each_with_index do |state_rule, index|
        rule = rules[index][:rule]
        unless state_rule["fingerprint"] == rules[index][:fingerprint]
          raise ArgumentError, "IceCube checkpoint does not match the current schedule"
        end

        rule.restore_state(
          state_rule["uses"].to_i,
          search_time,
          @schedule.start_time
        )
      end
    end

    def current_state
      state_for(last_occurrence_time, @state["schedule_fingerprint"])
    end

    def state_for(last_time, fingerprint)
      {
        "version" => CHECKPOINT_VERSION,
        "schedule_fingerprint" => fingerprint,
        "opening_time" => serialize_time(@opening_time),
        "closing_time" => serialize_time(@closing_time),
        "spans" => @spans,
        "last_occurrence_time" => serialize_time(last_time),
        "recurrence_rules" => rule_states(recurrence_rule_contexts),
        "exception_rules" => rule_states(exception_rule_contexts)
      }
    end

    def recurrence_rule_contexts
      @schedule.send(:recurrence_rules_with_implicit_start_occurrence).map do |rule|
        rule_context(rule)
      end
    end

    def exception_rule_contexts
      @schedule.instance_variable_get(:@all_exception_rules).map do |rule|
        rule_context(rule)
      end
    end

    def rule_context(rule)
      {
        rule: rule,
        fingerprint: fingerprint(rule.to_hash)
      }
    end

    def rule_states(rules)
      rules.map do |context|
        {
          "fingerprint" => context[:fingerprint],
          "uses" => context[:rule].uses.to_i
        }
      end
    end

    def last_occurrence_time
      deserialize_time(@state["last_occurrence_time"])
    end

    def verify_schedule_fingerprint(fingerprint)
      return unless @state["schedule_fingerprint"]
      return if fingerprint == @state["schedule_fingerprint"]

      raise ArgumentError, "IceCube checkpoint does not match the current schedule"
    end

    def schedule_fingerprint
      fingerprint(@schedule.to_hash)
    end

    def state_with_fingerprint(state)
      state.merge(
        "checkpoint_fingerprint" => fingerprint(state.reject { |key| key == "checkpoint_fingerprint" })
      )
    end

    def load_checkpoint(checkpoint)
      parsed = checkpoint.is_a?(String) ? JSON.parse(checkpoint) : JSON.parse(JSON.generate(checkpoint))
      unless parsed.is_a?(Hash) && parsed["version"] == CHECKPOINT_VERSION
        raise ArgumentError, "Unsupported IceCube checkpoint version"
      end
      verify_required_fields(parsed)

      verify_checkpoint_fingerprint(parsed)
      verify_rule_group(parsed["recurrence_rules"], recurrence_rule_contexts)
      verify_rule_group(parsed["exception_rules"], exception_rule_contexts)
      parsed
    rescue JSON::ParserError
      raise ArgumentError, "Invalid IceCube checkpoint"
    end

    def initial_state(fingerprint)
      {
        "version" => CHECKPOINT_VERSION,
        "schedule_fingerprint" => fingerprint,
        "opening_time" => serialize_time(@opening_time),
        "closing_time" => serialize_time(@closing_time),
        "spans" => @spans,
        "last_occurrence_time" => nil,
        "recurrence_rules" => rule_states(recurrence_rule_contexts),
        "exception_rules" => rule_states(exception_rule_contexts)
      }
    end

    def verify_checkpoint_fingerprint(state)
      expected = fingerprint(state.reject { |key| key == "checkpoint_fingerprint" })
      return if state["checkpoint_fingerprint"] == expected

      raise ArgumentError, "Invalid IceCube checkpoint"
    end

    def verify_boundaries(state, requested_spans)
      requested = requested_spans == true && @schedule.duration != 0
      if state["spans"] != requested
        raise ArgumentError, "IceCube checkpoint does not match the current enumeration boundaries"
      end
    end

    def verify_requested_boundary(name, requested, checkpoint_value)
      return if requested.nil?
      return if serialize_time(requested) == serialize_time(checkpoint_value)

      raise ArgumentError, "IceCube checkpoint does not match the current #{name}"
    end

    def verify_schedule_fingerprint_value(fingerprint_value)
      return if fingerprint_value == schedule_fingerprint

      raise ArgumentError, "IceCube checkpoint does not match the current schedule"
    end

    def verify_rule_group(state_rules, rules)
      unless state_rules.is_a?(Array) && state_rules.length == rules.length
        raise ArgumentError, "IceCube checkpoint does not match the current schedule"
      end

      state_rules.each_with_index do |state_rule, index|
        valid = state_rule.is_a?(Hash) &&
          state_rule["fingerprint"] == rules[index][:fingerprint] &&
          state_rule["uses"].is_a?(::Integer) &&
          state_rule["uses"] >= 0
        next if valid

        raise ArgumentError, "IceCube checkpoint does not match the current schedule"
      end
    end

    def verify_required_fields(state)
      required = %w[
        schedule_fingerprint opening_time closing_time spans
        last_occurrence_time recurrence_rules exception_rules
      ]
      valid = required.all? { |key| state.key?(key) } &&
        state["schedule_fingerprint"].is_a?(String) &&
        [true, false].include?(state["spans"]) &&
        (state["last_occurrence_time"].nil? || state["last_occurrence_time"].is_a?(String))
      return if valid

      raise ArgumentError, "Invalid IceCube checkpoint"
    end

    def normalize_opening_time(time)
      time = TimeUtil.match_zone(time, @schedule.start_time)
      time += TimeUtil.subsec(@schedule.start_time) - TimeUtil.subsec(time)
      time < @schedule.start_time ? @schedule.start_time : time
    end

    def serialize_time(time)
      return nil if time.nil?

      case time
      when DateTime
        TimeUtil.ensure_time(time).getutc.strftime("%Y-%m-%dT%H:%M:%S.%9NZ")
      when Time
        time.getutc.strftime("%Y-%m-%dT%H:%M:%S.%9NZ")
      when Date
        time.iso8601
      when Object
        if time.respond_to?(:utc) && time.respond_to?(:nsec)
          canonical_time(time.utc)
        else
          raise ArgumentError, "Invalid time in IceCube checkpoint: #{time.inspect}"
        end
      else
        raise ArgumentError, "Invalid time in IceCube checkpoint: #{time.inspect}"
      end
    end

    def deserialize_time(value)
      return nil if value.nil?

      raise ArgumentError, "Invalid IceCube checkpoint" unless value.is_a?(String)

      unless value.match?(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z\z/)
        raise ArgumentError, "Invalid IceCube checkpoint"
      end

      TimeUtil.match_zone(Time.iso8601(value), @schedule.start_time)
    end

    def fingerprint(value)
      Digest::SHA256.hexdigest(JSON.generate(canonical_value(value)))
    end

    def canonical_value(value)
      case value
      when Hash
        value.keys.sort_by(&:to_s).each_with_object({}) do |key, result|
          result[key.to_s] = canonical_value(value[key])
        end
      when Array
        value.map { |item| canonical_value(item) }
      when Symbol
        value.to_s
      when DateTime
        canonical_time(TimeUtil.ensure_time(value))
      when Time
        canonical_time(value)
      when Date
        value.iso8601
      when String, ::Integer, Float, true, false, nil
        value
      else
        if value.respond_to?(:utc) && value.respond_to?(:nsec)
          canonical_time(value.utc)
        else
          raise ArgumentError, "Unsafe value in IceCube checkpoint: #{value.inspect}"
        end
      end
    end

    def canonical_time(time)
      time.getutc.strftime("%Y-%m-%dT%H:%M:%S.%9NZ")
    end

    def deep_dup(value)
      case value
      when Hash
        value.each_with_object({}) { |(key, item), result| result[key] = deep_dup(item) }
      when Array
        value.map { |item| deep_dup(item) }
      else
        value
      end
    end
  end
end
