# frozen_string_literal: true

RSpec.describe "Per-call context" do
  let(:undefined) { Dry::Core::Constants::Undefined }

  # A money amount converted using the unit table carried by the call.
  # Two tables may use the same unit names with different rates.
  let(:money) do
    Dry::Types["hash"].schema(
      amount: Dry::Types["coercible.integer"],
      currency: Dry::Types["strict.string"]
    ).constructor(context: true) do |input, units|
      units = undefined.equal?(units) ? {} : units
      rate = units.fetch(input.fetch(:currency))
      input.merge(amount: input.fetch(:amount) * rate, currency: "BASE")
    end
  end

  describe "constructor declaration" do
    it "passes the context to a (input, context) constructor" do
      seen = nil
      type = Dry::Types["any"].constructor(context: true) do |input, context|
        seen = context
        input
      end

      type.call(1, :the_context)

      expect(seen).to be(:the_context)
    end

    it "passes the context to a (input, type, context) wrapper constructor" do
      arguments = nil
      type = Dry::Types["integer"].constructor(context: true) do |input, type, context|
        arguments = [input, type, context]
        type.call(input)
      end

      type.call(1, :the_context)

      expect(arguments).to eql([1, Dry::Types["integer"], :the_context])
    end

    it "keeps calling an ordinary value-only constructor the old way" do
      received = []
      type = Dry::Types["any"].constructor do |input|
        received << input
        input
      end

      expect(type.call(1, :ignored_context)).to be(1)
      expect(received).to eql([1])
    end

    it "supports Method objects" do
      coercer = Class.new {
        def call(value, table)
          (value.to_i * table.fetch(:mul)).to_s
        end
      }.new

      type = Dry::Types["any"].constructor(coercer.method(:call), context: true)

      expect(type.call(2, {mul: 3})).to eql("6")
    end

    it "supports block-yielding Method objects" do
      coercer = Class.new {
        def call(value, table)
          Integer(value) * table.fetch(:mul)
        rescue ArgumentError
          yield
        end
      }.new

      type = Dry::Types["any"].constructor(coercer.method(:call), context: true)

      expect(type.call("2", {mul: 3}) { :fallback }).to be(6)
      expect(type.call("x", {mul: 3}) { :fallback }).to be(:fallback)
    end

    it "does not expose the context on the frozen type instance" do
      type = Dry::Types["any"].constructor(context: true) { |v, c| [v, c] }

      expect(type).to be_frozen
      expect(type.instance_variables).not_to include(:@context)
      expect(type).not_to respond_to(:context)
    end
  end

  describe "propagation" do
    it "threads the context through nested schemas and arrays" do
      schema = Dry::Types["hash"].schema(
        payments: Dry::Types["array"].of(money)
      )

      table = { "USD" => 10, "EUR" => 20 }
      result = schema.call(
        { payments: [{amount: 1, currency: "USD"}, {amount: 2, currency: "EUR"}] },
        table
      )

      expect(result.fetch(:payments)).to eql(
        [
          {amount: 10, currency: "BASE"},
          {amount: 40, currency: "BASE"}
        ]
      )
    end

    it "threads the context through a wrapper that does not declare it" do
      contextual = Dry::Types["integer"].constructor(context: true) do |value, table|
        value * table.fetch(:mul)
      end

      # A regular (input, type) wrapper invokes the inner type itself;
      # the context still reaches the contextual constructor below
      wrapped = contextual.constructor { |input, type| type.call(input) + 100 }

      expect(wrapped.call(2, {mul: 5})).to be(110)
    end

    it "threads the context through a prepended wrapper" do
      contextual = Dry::Types["integer"].constructor(context: true) do |value, table|
        value * table.fetch(:mul)
      end
      wrapping = contextual.constructor { |input, type| type.call(input) + 100 }

      with_context = wrapping.prepend(context: true) do |input, table|
        input * table.fetch(:pre)
      end

      # 2 * 3 -> 6 -> *5 = 30 -> +100 = 130
      expect(with_context.call(2, {pre: 3, mul: 5})).to be(130)
    end

    it "threads the context through sum branches" do
      fixed = Dry::Types["hash"].schema(
        kind: Dry::Types["strict.string"].enum("fixed"),
        value: Dry::Types["integer"]
      ).constructor(context: true) { |h, t| h.merge(value: h.fetch(:value) * t.fetch(:fixed)) }

      percent = Dry::Types["hash"].schema(
        kind: Dry::Types["strict.string"].enum("percent"),
        value: Dry::Types["integer"]
      ).constructor(context: true) { |h, t| h.merge(value: h.fetch(:value) * t.fetch(:percent)) }

      sum = fixed | percent
      table = { fixed: 2, percent: 3 }

      expect(sum.call({kind: "fixed", value: 10}, table).fetch(:value)).to be(20)
      expect(sum.call({kind: "percent", value: 10}, table).fetch(:value)).to be(30)
    end

    it "threads the context through evaluated defaults" do
      callable_default = Dry::Types["hash"].schema(
        currency: Dry::Types["strict.string"]
      ).default { |_type, context| {currency: "DEF", rate: context.fetch(:rate)} }

      schema = Dry::Types["hash"].schema(detail: callable_default)

      expect(schema.call({}, {rate: 42}).fetch(:detail)).to eql(
        currency: "DEF", rate: 42
      )
    end

    it "still evaluates plain defaults normally" do
      schema = Dry::Types["hash"].schema(
        currency: Dry::Types["strict.string"].default("USD")
      )

      expect(schema.call({}, {anything: true})).to eql(currency: "USD")
    end

    it "passes the context to #try as well" do
      seen = nil
      type = Dry::Types["any"].constructor(context: true) do |input, context|
        seen = context
        input
      end

      result = type.try(1, :try_context)

      expect(result).to be_success
      expect(seen).to be(:try_context)
    end
  end

  describe "isolation" do
    let(:table_a) { { "USD" => 100, "EUR" => 110 } }
    let(:table_b) { { "USD" => 2, "EUR" => 3 } }

    let(:deep) do
      fixed = Dry::Types["hash"].schema(
        kind: Dry::Types["strict.string"].enum("fixed"),
        value: Dry::Types["integer"]
      )
      percent = Dry::Types["hash"].schema(
        kind: Dry::Types["strict.string"].enum("percent"),
        value: Dry::Types["integer"]
      )

      choice = (fixed | percent).default(
        -> _type { {kind: "fixed", value: 0} }.freeze
      )

      Dry::Types["hash"].schema(
        payments: Dry::Types["array"].of(money),
        total: money,
        choice: choice
      )
    end

    let(:input) do
      {
        payments: [
          {amount: 1, currency: "USD"},
          {amount: 2, currency: "EUR"}
        ],
        total: {amount: 5, currency: "EUR"}
      }
    end

    it "converts the same deep structure differently per request table" do
      result_a = deep.call(input, table_a)
      result_b = deep.call(input, table_b)

      aggregate_failures do
        expect(result_a.fetch(:payments)).to eql(
          [
            {amount: 100, currency: "BASE"},
            {amount: 220, currency: "BASE"}
          ]
        )
        expect(result_b.fetch(:payments)).to eql(
          [
            {amount: 2, currency: "BASE"},
            {amount: 6, currency: "BASE"}
          ]
        )
        expect(result_a.fetch(:total)).to eql(amount: 550, currency: "BASE")
        expect(result_b.fetch(:total)).to eql(amount: 15, currency: "BASE")
      end
    end

    it "leaves no context on the type after a call" do
      deep.call(input, table_a)

      # Context is purely a call-stack argument: the shared frozen types
      # keep no reference to it, so a subsequent call receives exactly
      # the context it passes (here Undefined, exercising the fallback).
      type = deep
      type.call(input, table_b)

      expect(type.instance_variables).not_to include(:@context)
      expect(money).to be_frozen
    end

    it "does not leave the context behind when coercion fails" do
      bad_input = input.merge(total: {amount: 1, currency: "JPY"})

      expect { deep.call(bad_input, table_a) }.to raise_error(StandardError)
      expect(deep.instance_variables).not_to include(:@context)

      # type remains fully usable with another table
      expect(deep.call(input, table_b).fetch(:total)).to eql(
        amount: 15, currency: "BASE"
      )
    end

    it "keeps two Fiber-interleaved deep conversions from crossing values" do
      # The constructor yields control after the input is read but before
      # the rate is applied, so the two conversions overlap while sharing
      # the exact same type instances.
      interleaved = Dry::Types["hash"].schema(
        amount: Dry::Types["integer"],
        currency: Dry::Types["strict.string"]
      ).constructor(context: true) do |input, units|
        Fiber.yield
        rate = units.fetch(input.fetch(:currency))
        input.merge(amount: input.fetch(:amount) * rate, currency: "BASE")
      end

      schema = Dry::Types["hash"].schema(list: Dry::Types["array"].of(interleaved))

      results = {}
      fiber_a = Fiber.new do
        results[:a] = schema.call({list: [{amount: 3, currency: "USD"}]}, table_a)
      end
      fiber_b = Fiber.new do
        results[:b] = schema.call({list: [{amount: 3, currency: "USD"}]}, table_b)
      end

      fiber_a.resume # parks inside A's constructor
      fiber_b.resume # parks inside B's constructor
      fiber_a.resume # resumes A against table_a
      fiber_b.resume # resumes B against table_b

      expect(results).to eql(
        a: {list: [{amount: 300, currency: "BASE"}]},
        b: {list: [{amount: 6, currency: "BASE"}]}
      )
    end

    it "keeps many thread-interleaved conversions from crossing values" do
      results = Queue.new
      input = {list: [{amount: 1, currency: "USD"}]}
      schema = Dry::Types["hash"].schema(list: Dry::Types["array"].of(money))

      threads = Array.new(20) do |index|
        Thread.new do
          table = index.even? ? table_a : table_b
          20.times do
            results << schema.call(input, table).dig(:list, 0, :amount)
          end
        end
      end
      threads.each(&:join)

      amounts = Array.new(results.size) { results.pop }

      aggregate_failures do
        expect(amounts).to all(be_a(Integer))
        expect(amounts.uniq.sort).to eql([2, 100])
        expect(amounts.count(100)).to be(200)
        expect(amounts.count(2)).to be(200)
      end
    end
  end
end
