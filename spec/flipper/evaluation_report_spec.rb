require "spec_helper"

RSpec.describe Flipper::EvaluationReport do
  let(:flipper) { Flipper.new(Flipper::Adapters::Memory.new) }
  let(:feature) { flipper[:search] }

  # An actor whose group predicates and property reads are counted, so examples
  # can prove exactly how often each input was touched during one explanation.
  let(:actor_class) do
    Class.new do
      attr_reader :admin_calls, :age_calls

      def initialize(id, admin:, age:)
        @id, @admin, @age = id, admin, age
        @admin_calls = 0
        @age_calls = 0
      end

      def flipper_id
        @id
      end

      def flipper_properties
        { "age" => @age }
      end

      def admin?
        @admin_calls += 1
        @admin
      end

      def age
        @age_calls += 1
        @age
      end
    end
  end

  describe "#result vs enabled?" do
    it "matches the boolean enabled? returns when fully on" do
      feature.enable
      actor = Flipper::Actor.new("User;1")
      report = feature.explain(actor)

      expect(report).to be_result
      expect(report.result).to eq(feature.enabled?(actor))
      expect(report.open_gate_name).to eq(:boolean)
    end

    it "matches the boolean enabled? returns when fully off" do
      feature.disable
      actor = Flipper::Actor.new("User;1")
      report = feature.explain(actor)

      expect(report.result).to eq(false)
      expect(report.result).to eq(feature.enabled?(actor))
      expect(report.open_gate_name).to be_nil
    end
  end

  describe "gate order and short circuiting" do
    it "records every gate in the real order" do
      feature.disable
      report = feature.explain(Flipper::Actor.new("User;1"))

      expect(report.entries.map(&:name)).to eq(
        [:boolean, :expression, :actor, :percentage_of_actors, :percentage_of_time, :group]
      )
    end

    it "marks gates after the first open gate as skipped without calling them" do
      feature.enable
      report = feature.explain(Flipper::Actor.new("User;1"))

      expect(report.evaluated_gate_names).to eq([:boolean])
      expect(report.skipped_gate_names).to eq(
        [:expression, :actor, :percentage_of_actors, :percentage_of_time, :group]
      )
      report.entries.select(&:skipped?).each do |entry|
        expect(entry.result).to be_nil
        expect(entry.details[:reason]).to eq(:previous_gate_open)
      end
    end

    it "skips actor-only gates when no actor is supplied" do
      feature.enable_actor(Flipper::Actor.new("User;7"))
      report = feature.explain

      expect(report.gate(:actor)).to be_skipped
      expect(report.gate(:actor).details[:reason]).to eq(:no_actors)
    end
  end

  describe "group gate" do
    it "calls the group block once and records the check" do
      Flipper.register(:spec_admins) { |actor| actor.admin? }
      feature.enable_group(:spec_admins)
      actor = actor_class.new("User;1", admin: true, age: 30)

      report = feature.explain(actor)

      expect(actor.admin_calls).to eq(1)
      expect(report.gate(:group).result).to eq(true)
      check = report.gate(:group).details[:checks].first
      expect(check[:group]).to eq("spec_admins")
      expect(check[:result]).to eq(true)
    end

    it "preserves the real short circuit across enabled groups" do
      calls = []
      Flipper.register(:spec_first) { |_actor| calls << :first; false }
      Flipper.register(:spec_second) { |_actor| calls << :second; true }
      Flipper.register(:spec_third) { |_actor| calls << :third; true }
      feature.enable_group(:spec_first)
      feature.enable_group(:spec_second)
      feature.enable_group(:spec_third)

      report = feature.explain(Flipper::Actor.new("User;1"))

      expect(calls).to eq([:first, :second])
      expect(report.open_gate_name).to eq(:group)
      checked = report.gate(:group).details[:checks].map { |c| c[:group].to_sym }
      expect(checked).to eq([:spec_first, :spec_second])
    end

    it "never runs the block when an earlier gate opens" do
      Flipper.register(:spec_late) { |actor| actor.admin? }
      feature.enable_group(:spec_late)
      feature.enable_percentage_of_time(50)
      actor = actor_class.new("User;1", admin: false, age: 30)

      report = feature.explain(actor, random: ->(_max = nil) { 0.01 })

      expect(report.open_gate_name).to eq(:percentage_of_time)
      expect(report.gate(:group)).to be_skipped
      expect(actor.admin_calls).to eq(0)
    end
  end

  describe "injected clock and random sources" do
    it "uses the injected random for the percentage of time gate and records it" do
      feature.enable_percentage_of_time(50)

      open_report = feature.explain(random: ->(_max = nil) { 0.01 })
      closed_report = feature.explain(random: ->(_max = nil) { 0.99 })

      expect(open_report.result).to eq(true)
      expect(open_report.gate(:percentage_of_time).details[:random]).to eq(0.01)
      expect(closed_report.result).to eq(false)
      expect(closed_report.gate(:percentage_of_time).details[:random]).to eq(0.99)
    end

    it "calls the bare percentage-of-time random source with no argument" do
      seen = :not_set
      feature.enable_percentage_of_time(50)

      feature.explain(random: ->(max = nil) { seen = max; 0.99 })

      expect(seen).to be_nil
    end

    it "does not read the clock on an Any branch that short circuits" do
      clock_calls = 0
      expression = Flipper.any(
        Flipper.property(:age).gte(18),
        Flipper.now.lte(Flipper.time("2020-01-01"))
      )
      feature.enable_expression(expression)
      actor = actor_class.new("User;1", admin: false, age: 30)

      report = feature.explain(actor, clock: -> { clock_calls += 1; Time.now.utc })

      expect(report.result).to eq(true)
      expect(clock_calls).to eq(0)
      node_names = expression_nodes(report).map(&:name)
      expect(node_names).to eq(["Property", "GreaterThanOrEqualTo", "Any"])
    end

    it "does not read random on an All branch that short circuits" do
      random_calls = 0
      expression = Flipper.all(
        Flipper.property(:age).lt(10),
        Flipper.random(100).lt(50)
      )
      feature.enable_expression(expression)
      actor = actor_class.new("User;1", admin: false, age: 30)

      report = feature.explain(actor, random: ->(max = nil) { random_calls += 1; 0.5 })

      expect(report.result).to eq(false)
      expect(random_calls).to eq(0)
      expect(expression_nodes(report).map(&:name)).to eq(["Property", "LessThan", "All"])
    end

    it "reads each property only when its branch runs" do
      expression = Flipper.any(
        Flipper.property(:age).gte(18),
        Flipper.property(:age).gte(99)
      )
      feature.enable_expression(expression)
      actor = actor_class.new("User;1", admin: false, age: 30)

      report = feature.explain(actor)

      expect(actor.age_calls).to eq(1)
      properties_seen = expression_nodes(report).count { |node| node.name == "Property" }
      expect(properties_seen).to eq(1)
    end

    it "passes the real Random argument to the injected source" do
      seen = []
      expression = Flipper.random(100).gte(50)
      feature.enable_expression(expression)

      report = feature.explain(
        Flipper::Actor.new("User;1"),
        random: ->(max = nil) { seen << max; 10 }
      )

      expect(seen).to eq([100])
      expect(report.result).to eq(false)
    end
  end

  describe "expression evaluation fidelity" do
    it "matches native evaluation for property-only expressions across actors" do
      expressions = [
        Flipper.property(:age).gte(18),
        Flipper.any(Flipper.property(:age).lt(5), Flipper.property(:age).gte(65)),
        Flipper.all(Flipper.property(:age).gte(0), Flipper.property(:age).lt(120)),
      ]

      [1, 10, 18, 65, 200].each do |age|
        actor = Flipper::Actor.new("User;#{age}", "age" => age)
        expressions.each do |expression|
          feature.clear
          feature.enable_expression(expression)
          expect(feature.explain(actor).result).to(
            eq(feature.enabled?(actor)),
            "expression #{expression.value.inspect} age=#{age}"
          )
        end
      end
    end

    it "follows the injected clock for a time-gated expression" do
      expression = Flipper.now.gte(Flipper.time("2026-06-01"))
      feature.enable_expression(expression)
      actor = Flipper::Actor.new("User;1")

      before_report = feature.explain(actor, clock: -> { Time.utc(2026, 1, 1) })
      after_report = feature.explain(actor, clock: -> { Time.utc(2026, 12, 1) })

      expect(before_report.result).to eq(false)
      expect(after_report.result).to eq(true)
    end

    it "records nodes for an expression checked without an actor" do
      feature.enable_expression(Flipper.now.gte(Flipper.time("2020-01-01")))

      report = feature.explain(clock: -> { Time.utc(2030) })

      expect(report.result).to eq(true)
      expect(report.gate(:expression).details[:nodes].map(&:name)).to(
        eq(["Now", "Time", "GreaterThanOrEqualTo"])
      )
    end

    it "records per-actor expression results" do
      feature.enable_expression(Flipper.property(:age).gte(18))
      young = Flipper::Actor.new("User;1", "age" => 10)
      old = Flipper::Actor.new("User;2", "age" => 40)

      report = feature.explain(young, old)
      results = report.gate(:expression).details[:actor_results]

      expect(results.map { |r| [r[:actor], r[:result]] }).to(
        eq([["User;1", false], ["User;2", true]])
      )
      expect(report.result).to eq(feature.enabled?(young, old))
    end

    it "closes the gate and records the reason for an unknown function" do
      feature.adapter.add(feature)
      feature.adapter.get(feature)[:expression] = { "FutureFunction" => [] }

      report = feature.explain(Flipper::Actor.new("User;1"))

      expect(report.result).to eq(false)
      expect(report.gate(:expression).details[:reason]).to eq(:unknown_function)
    end
  end

  describe "entry points" do
    it "is available through the DSL and the Flipper module" do
      feature.enable
      expect(flipper.explain(:search)).to be_a(described_class)
      Flipper.instance = flipper
      expect(Flipper.explain(:search)).to be_a(described_class)
    ensure
      Flipper.instance = nil
    end

    it "does not attach a report to the normal enabled? context" do
      feature.enable
      context = Flipper::FeatureCheckContext.new(
        feature_name: :search,
        values: feature.gate_values,
        actors: [Flipper::Types::Actor.new(Flipper::Actor.new("1"))]
      )

      expect(context.report).to be_nil
    end
  end

  def expression_nodes(report)
    details = report.gate(:expression).details
    details[:nodes] || details[:actor_results].flat_map { |result| result[:nodes] }
  end
end
