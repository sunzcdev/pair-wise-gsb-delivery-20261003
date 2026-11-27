module Flipper
  # Public: A single-evaluation explanation of why a feature was open or closed
  # for a given set of actors. Built by Feature#explain (never by the normal
  # enabled? path), it preserves the real gate order, the boolean each gate
  # actually returned, and only the inputs that were genuinely read while
  # checking. Gates after the first open gate are not evaluated, mirroring
  # Feature#enabled?'s `gates.detect { |gate| gate.open?(context) }`, so the
  # report never invokes a group block, property, clock, or random source a
  # real check would not have invoked.
  class EvaluationReport
    # Public: A single gate check. :evaluated gates were actually run and carry
    # the boolean their open? check returned; :skipped gates were never reached
    # because an earlier gate opened (or, for gates that cannot run without an
    # actor, because no actor was supplied) and carry no inputs.
    GateEntry = Struct.new(:name, :status, :result, :details, keyword_init: true) do
      def evaluated?
        status == :evaluated
      end

      def skipped?
        status == :skipped
      end
    end

    # Public: The Symbol name of the feature.
    attr_reader :feature_name

    # Public: The Array of Types::Actor instances the check ran for, or nil.
    attr_reader :actors

    # Public: The GateValues snapshot the check used.
    attr_reader :gate_values

    # Public: The callable used as the clock source for Now expressions, or nil
    # for the real Time.now.utc.
    attr_reader :clock

    # Public: The callable used as the random source for Random expressions and
    # the percentage of time gate, or nil for Kernel#rand.
    attr_reader :random

    def initialize(feature_name:, values:, actors: nil, clock: nil, random: nil)
      @feature_name = feature_name
      @gate_values = values
      @actors = actors
      @clock = clock
      @random = random
      @entries = []
      @entries_by_name = {}
    end

    # Public: The gate entries in the order the gates were considered. Entries
    # for skipped gates are included so the full gate order stays visible.
    #
    # Returns an Array of GateEntry.
    def entries
      @entries.dup
    end

    # Public: Find a gate's entry by gate name.
    #
    # Returns a GateEntry or nil if the gate was never reached.
    def gate(name)
      @entries_by_name[name.to_sym]
    end

    # Public: The Symbol name of the gate that opened, or nil if none did.
    def open_gate_name
      open_entry&.name
    end

    # Public: The entry of the gate that opened, or nil.
    def open_entry
      @entries.find { |entry| entry.evaluated? && entry.result }
    end

    # Public: The boolean the gate evaluation produced. Identical to what
    # Feature#enabled? returned for the same inputs.
    def result
      !open_entry.nil?
    end
    alias_method :enabled?, :result

    # Public: Names of the gates that were actually evaluated, in order.
    def evaluated_gate_names
      @entries.select(&:evaluated?).map(&:name)
    end

    # Public: Names of gates that were never reached.
    def skipped_gate_names
      @entries.select(&:skipped?).map(&:name)
    end

    # Internal: Record that a gate was checked and the boolean it returned.
    # Called by the gates themselves while an explanation is running.
    def record_gate(name, result, details = nil)
      entry = GateEntry.new(
        name: name.to_sym,
        status: :evaluated,
        result: !!result,
        details: details
      )
      @entries << entry
      @entries_by_name[entry.name] = entry
      entry
    end

    # Internal: Record a gate that was never evaluated. Used for the gates
    # remaining after an earlier gate opened, and for actor-only gates when the
    # check had no actors. No group blocks, properties, clock, or random source
    # are touched for these.
    def skip_gate(name, reason = nil)
      entry = GateEntry.new(
        name: name.to_sym,
        status: :skipped,
        result: nil,
        details: reason ? { reason: reason } : nil
      )
      @entries << entry
      @entries_by_name[entry.name] = entry
      entry
    end

    # Public: Read the random source, honoring an injected random source. The
    # percentage of time gate draws through here; Random expression nodes go
    # through the ExpressionEvaluator, which applies the same source.
    def read_random(max = nil)
      if @random
        @random.call(max)
      else
        rand(max)
      end
    end

    # Public: Build the evaluation context an expression gate needs, pointing
    # Now/Random at this report's injected sources.
    def expression_context(feature_name:, properties:, actor:)
      {
        feature_name: feature_name,
        properties: properties,
        actor: actor,
        report: self
      }
    end

    # Public: Human-readable rendering of the decision and the gates involved.
    def to_s
      lines = []
      lines << "#{feature_name} #{result ? 'enabled' : 'disabled'}"
      lines << "  actors: #{(actors || []).map(&:value).inspect}"
      @entries.each do |entry|
        if entry.evaluated?
          lines << "  #{entry.name}: #{entry.result} (#{summarize_details(entry.details)})"
        else
          lines << "  #{entry.name}: skipped (#{entry.details&.dig(:reason) || 'not evaluated'})"
        end
      end
      lines.join("\n")
    end
    alias_method :inspect, :to_s

    private

    def summarize_details(details)
      return "no details" if details.nil? || details.empty?

      details.map { |key, value| "#{key}=#{value.inspect}" }.join(", ")
    end
  end
end
