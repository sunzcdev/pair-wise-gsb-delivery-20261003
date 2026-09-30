require "json"
require "digest"
require "time"

module IceCube
  # A resumable iterator over schedule occurrences.
  #
  # Unlike an Enumerator produced by the schedule, an OccurrenceIterator can
  # export its continuation position with #checkpoint (or #to_json), which may
  # be persisted by a background scanner and restored later with
  # OccurrenceIterator.resume (or Schedule#resume_occurrence_iterator).
  #
  # The checkpoint only contains JSON-safe primitives (String, Integer,
  # Boolean, nil, Array, Hash); it never uses Marshal and can never load
  # arbitrary objects. It is bound to:
  #
  # * a format version,
  # * the schedule content (start/end time, rules and individual times),
  # * the enumeration boundaries (opening/closing time and +:spans+),
  # * and each rule's definition.
  #
  # Resuming rejects a checkpoint when any of those change. On resume the
  # iterator continues strictly after the last emitted occurrence, so the last
  # item is never repeated and rule COUNT budgets are not charged again.
  class OccurrenceIterator
    include Enumerable

    # Bumped whenever the checkpoint representation changes.
    FORMAT_VERSION = 1

    # Resume an iterator previously exported for +schedule+.
    # +checkpoint+ may be the checkpoint Hash or its JSON String.
    def self.resume(schedule, checkpoint)
      data = decode_checkpoint(checkpoint)
      raise InvalidCheckpoint, "Malformed resume checkpoint" unless data.is_a?(Hash)

      new(schedule, nil, nil, resume: data)
    end

    # Start a fresh iterator. Defaults to iterating from the schedule start
    # time with no closing boundary.
    def initialize(schedule, opening_time = nil, closing_time = nil, options = {})
      @schedule = schedule
      if (data = options[:resume])
        restore!(data, opening_time, closing_time, options)
      else
        start!(opening_time, closing_time, options[:spans] == true)
      end
    end

    # True once the closing boundary has been reached or every terminating
    # rule has been exhausted.
    def exhausted?
      @exhausted
    end

    # Return the next Occurrence, or nil when exhausted.
    def next_occurrence
      advance
    end

    # Return the next +num+ occurrences (fewer when exhausting early).
    def next_occurrences(num)
      Array.new(num) { next_occurrence }.compact
    end

    # Yield every occurrence from the current position until exhausted.
    # A non-terminating schedule without a closing time iterates forever.
    def each
      return to_enum(__method__) unless block_given?

      while (occurrence = advance)
        yield occurrence
      end
      self
    end

    # Export the continuation position as a JSON-safe Hash.
    def checkpoint
      {
        "version" => FORMAT_VERSION,
        "schedule" => schedule_signature,
        "opening_time" => encode_time(@opening_time),
        "closing_time" => encode_time(@closing_time),
        "spans" => @spans,
        "cursor" => encode_time(@cursor),
        "exhausted" => @exhausted,
        "rules" => rule_checkpoints
      }
    end
    alias_method :to_h, :checkpoint

    # Export the continuation position as a JSON String.
    def to_json(*args)
      JSON.generate(checkpoint, *args)
    end

    private

    # Reset every rule and position at the opening boundary.
    def start!(opening_time, closing_time, spans)
      start_time = @schedule.start_time
      @opening_time = TimeUtil.match_zone(opening_time || start_time, start_time)
      @closing_time = TimeUtil.match_zone(closing_time, start_time)
      @opening_time += TimeUtil.subsec(start_time) - TimeUtil.subsec(@opening_time)
      @opening_time = start_time if @opening_time < start_time
      @spans = spans && @schedule.duration != 0

      @schedule.send(:reset)
      @cursor = @schedule.send(:full_required?) ? start_time : @opening_time
      @cursor -= @schedule.duration if @spans
      @cursor = start_time if @cursor < start_time
      @exhausted = false
    end

    # Validate +data+ against the current schedule and restore every rule's
    # enumeration state without charging any COUNT again. When the caller also
    # supplies boundaries or a +:spans+ option, they must match the values
    # captured by the checkpoint.
    def restore!(data, passed_opening = nil, passed_closing = nil, passed_options = {})
      verify_shape!(data)
      raise InvalidCheckpoint, "Unsupported checkpoint version: #{data["version"].inspect}" unless data["version"] == FORMAT_VERSION
      raise InvalidCheckpoint, "Checkpoint does not belong to this schedule" unless data["schedule"] == schedule_signature

      start_time = @schedule.start_time
      opening_time = decode_time(data["opening_time"], start_time)
      closing_time = decode_time(data["closing_time"], start_time)
      verify_boundaries!(data, passed_opening, passed_closing, passed_options, start_time)

      @schedule.send(:reset)
      restore_rules!(data["rules"])

      @opening_time = opening_time
      @closing_time = closing_time
      @spans = data["spans"] == true
      @cursor = decode_time(data["cursor"], start_time)
      @exhausted = data["exhausted"] == true
    end

    # The checkpoint is bound to its enumeration boundaries. Explicitly
    # supplied boundaries must agree with the checkpoint, otherwise it is
    # rejected as belonging to a different enumeration.
    def verify_boundaries!(data, passed_opening, passed_closing, passed_options, reference)
      if !passed_opening.nil?
        given = TimeUtil.match_zone(passed_opening, reference)
        unless data["opening_time"] == encode_time(given)
          raise InvalidCheckpoint, "Checkpoint was created for a different opening time"
        end
      end
      if !passed_closing.nil?
        given = TimeUtil.match_zone(passed_closing, reference)
        unless data["closing_time"] == encode_time(given)
          raise InvalidCheckpoint, "Checkpoint was created for a different closing time"
        end
      end
      if passed_options.key?(:spans) && data["spans"] != (passed_options[:spans] == true)
        raise InvalidCheckpoint, "Checkpoint was created with a different spans option"
      end
    end

    # One iteration step, mirroring Schedule#enumerate_occurrences so paged
    # results are identical to a single enumeration. Returns an Occurrence or
    # nil. Advances the cursor for every candidate, including candidates that
    # fall before the opening boundary.
    def advance
      return nil if @exhausted

      loop do
        candidate = @schedule.send(:next_time, @cursor, @closing_time)
        if candidate.nil? || (@closing_time && candidate > @closing_time)
          @exhausted = true
          return nil
        end

        accepted = @spans ? candidate.end_time > @opening_time : candidate >= @opening_time
        @cursor = candidate + 1
        return candidate if accepted
      end
    end

    # All rules that carry mutable enumeration state, tagged by the list they
    # live in and their position within it.
    def rule_entries
      [:recurrence, :exception].flat_map do |list|
        rules = list == :recurrence ? @schedule.instance_variable_get(:@all_recurrence_rules) :
          @schedule.instance_variable_get(:@all_exception_rules)
        rules.each_with_index.map { |rule, index| [list, index, rule] }
      end
    end

    def rule_checkpoints
      rule_entries.map do |list, index, rule|
        entry = {
          "list" => list.to_s,
          "index" => index,
          "type" => rule.class.name,
          "signature" => signature_for(rule.to_hash)
        }
        if rule.respond_to?(:enumeration_state)
          state = rule.enumeration_state
          entry["uses"] = state[:uses].to_i
          entry["start_time"] = encode_time(state[:start_time])
        end
        entry
      end
    end

    # Restore states in the exact rule order captured by the checkpoint.
    def restore_rules!(stored)
      expected = rule_entries
      unless stored.is_a?(Array) && stored.length == expected.length
        raise InvalidCheckpoint, "Checkpoint rule set does not match this schedule"
      end

      expected.zip(stored).each do |(list, index, rule), entry|
        verify_rule_entry!(entry)
        unless entry["list"] == list.to_s &&
            entry["index"] == index &&
            entry["type"] == rule.class.name &&
            entry["signature"] == signature_for(rule.to_hash)
          raise InvalidCheckpoint, "Checkpoint was created against a different rule set"
        end
        next unless rule.respond_to?(:restore_enumeration_state)

        rule.restore_enumeration_state(
          uses: entry["uses"].to_i,
          start_time: decode_time(entry["start_time"], @schedule.start_time)
        )
      end
    end

    def verify_rule_entry!(entry)
      return if entry.is_a?(Hash) &&
        entry["list"].is_a?(String) &&
        entry["index"].is_a?(Integer) &&
        entry["type"].is_a?(String) &&
        entry["signature"].is_a?(String)

      raise InvalidCheckpoint, "Malformed checkpoint rule entry"
    end

    def schedule_signature
      signature_for(@schedule.to_hash)
    end

    # Deterministic SHA-256 over the canonical JSON of a plain data structure.
    def signature_for(object)
      Digest::SHA256.hexdigest(JSON.generate(canonicalize(object)))
    end

    # Convert Time/Date/Symbol and symbol-keyed hashes into stable JSON values.
    def canonicalize(object)
      case object
      when Hash
        object.each_with_object({}) do |(key, value), memo|
          memo[key.to_s] = canonicalize(value)
        end.sort.to_h
      when Array
        object.map { |value| canonicalize(value) }
      when Symbol
        object.to_s
      when Time, DateTime
        time_signature(object)
      when Date
        object.iso8601
      else
        object
      end
    end

    def time_signature(time)
      if time.respond_to?(:utc)
        time.utc.iso8601(9)
      else
        time.to_time.utc.iso8601(9)
      end
    end

    def encode_time(time)
      return nil if time.nil?

      time_signature(time)
    end

    def decode_time(value, reference)
      return nil if value.nil?
      raise InvalidCheckpoint, "Expected an ISO8601 time, got #{value.inspect}" unless value.is_a?(String)

      TimeUtil.match_zone(Time.iso8601(value), reference)
    rescue ArgumentError
      raise InvalidCheckpoint, "Unparseable checkpoint time: #{value.inspect}"
    end

    # Validate the top-level checkpoint strictly so foreign or hand-crafted
    # payloads cannot smuggle unexpected types into the iterator.
    def verify_shape!(data)
      valid = data.is_a?(Hash) &&
        data["version"].is_a?(Integer) &&
        data["schedule"].is_a?(String) &&
        (data["opening_time"].nil? || data["opening_time"].is_a?(String)) &&
        (data["closing_time"].nil? || data["closing_time"].is_a?(String)) &&
        [true, false].include?(data["spans"]) &&
        (data["cursor"].nil? || data["cursor"].is_a?(String)) &&
        [true, false].include?(data["exhausted"]) &&
        data["rules"].is_a?(Array)
      raise InvalidCheckpoint, "Malformed resume checkpoint" unless valid
    end

    # Parse a checkpoint Hash or JSON String without loading any objects.
    def self.decode_checkpoint(checkpoint)
      case checkpoint
      when Hash
        checkpoint
      when String
        JSON.parse!(checkpoint)
      else
        raise InvalidCheckpoint, "Checkpoint must be a Hash or a JSON String"
      end
    rescue JSON::ParserError
      raise InvalidCheckpoint, "Checkpoint is not valid JSON"
    end
  end
end
