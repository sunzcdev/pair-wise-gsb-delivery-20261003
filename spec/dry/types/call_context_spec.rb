# frozen_string_literal: true

RSpec.describe Dry::Types::CallContext do
  # Two unit tables with the same unit names but different conversion values
  let(:table_a) { {"USD" => 100, "EUR" => 200}.freeze }
  let(:table_b) { {"USD" => 7, "EUR" => 9}.freeze }

  let(:amount_hook) { -> {} }

  # One and the same amount type, reused in the whole tree. It converts
  # {value:, unit:} hashes to cents using the table carried by the call
  let(:amount_type) do
    hook = amount_hook

    Dry::Types["integer"].constructor(context: true) do |input, table|
      hook.call
      input[:value] * table.fetch(input[:unit])
    end
  end

  # Deep structure: nested schemas, an array, a default value and a sum branch
  let(:order_type) do
    amount = amount_type

    Dry::Types["hash"].schema(
      id: Dry::Types["integer"],
      amount: amount,
      lines: Dry::Types["array"].of(
        Dry::Types["hash"].schema(
          title: Dry::Types["string"],
          price: amount
        )
      ),
      discount: Dry::Types["string"] | amount,
      fee: Dry::Types["integer"].default(
        ->(_type) { Dry::Types::CallContext.current.fetch("USD") },
        shared: true
      )
    )
  end

  let(:input) do
    {
      id: 1,
      amount: {value: 5, unit: "USD"},
      lines: [{title: "book", price: {value: 2, unit: "EUR"}}],
      discount: {value: 3, unit: "EUR"}
    }
  end

  describe "#call_with_context" do
    it "converts a deep structure with the table passed for this call" do
      expect(order_type.call_with_context(input, context: table_a)).to eql(
        id: 1,
        amount: 500,
        lines: [{title: "book", price: 400}],
        discount: 600,
        fee: 100
      )
    end

    it "converts the same structure with a different table on another call" do
      expect(order_type.call_with_context(input, context: table_b)).to eql(
        id: 1,
        amount: 35,
        lines: [{title: "book", price: 18}],
        discount: 27,
        fee: 7
      )
    end

    it "rebuilds nothing between calls, the very same type instance is reused" do
      first = order_type.call_with_context(input, context: table_a)
      second = order_type.call_with_context(input, context: table_b)

      expect(first).not_to eql(second)
      expect(order_type.call_with_context(input, context: table_a)).to eql(first)
    end

    it "passes context through the matching sum branch" do
      output = order_type.call_with_context(input.merge(discount: "none"), context: table_a)

      expect(output[:discount]).to eql("none")
    end

    it "evaluates defaults within the call context" do
      expect(order_type.call_with_context(input, context: table_a)[:fee]).to eql(100)
      expect(order_type.call_with_context(input, context: table_b)[:fee]).to eql(7)
    end

    it "leaves no context behind after a successful call" do
      order_type.call_with_context(input, context: table_a)

      expect(Dry::Types::CallContext.current).to be_nil
      expect(Dry::Types::CallContext.current?).to be(false)
    end

    it "leaves no context behind after a failed call" do
      expect {
        order_type.call_with_context({id: "oops"}, context: table_a)
      }.to raise_error(Dry::Types::SchemaError)

      expect(Dry::Types::CallContext.current).to be_nil
    end

    it "does not leak the context into the type instance" do
      order_type.call_with_context(input, context: table_a)

      expect {
        amount_type.({value: 5, unit: "USD"})
      }.to raise_error(Dry::Types::CoercionError)
    end

    it "restores a previously set outer context" do
      Dry::Types::CallContext.with(table_a) do
        order_type.call_with_context(input, context: table_b)

        expect(Dry::Types::CallContext.current).to equal(table_a)
      end
    end

    it "keeps calling value-only constructors with just the value" do
      type = Dry::Types["integer"].constructor { |value| value.to_i }

      expect(type.call_with_context("5", context: table_a)).to eql(5)
      expect(type.("6")).to eql(6)
    end

    context "with a hook yielding from every amount conversion" do
      let(:amount_hook) { -> { Fiber.yield } }

      it "does not cross context between interleaved fibers" do
        results = {}

        fibers = {a: table_a, b: table_b}.map do |name, table|
          Fiber.new do
            results[name] = order_type.call_with_context(input, context: table)
          end
        end

        fibers.each(&:resume) while fibers.any?(&:alive?)

        expect(results[:a]).to eql(
          id: 1, amount: 500, lines: [{title: "book", price: 400}], discount: 600, fee: 100
        )
        expect(results[:b]).to eql(
          id: 1, amount: 35, lines: [{title: "book", price: 18}], discount: 27, fee: 7
        )
        expect(Dry::Types::CallContext.current).to be_nil
      end
    end

    context "with a hook pausing every amount conversion" do
      let(:amount_hook) { -> { sleep 0.01 } }

      it "does not cross context between concurrent threads" do
        results = {}

        threads = {a: table_a, b: table_b}.map do |name, table|
          Thread.new do
            results[name] = order_type.call_with_context(input, context: table)
          end
        end

        threads.each(&:join)

        expect(results[:a][:amount]).to eql(500)
        expect(results[:b][:amount]).to eql(35)
      end
    end
  end

  describe ".with" do
    it "makes the context available to #try and restores it afterwards" do
      result = Dry::Types::CallContext.with(table_a) do
        amount_type.try(value: 5, unit: "USD")
      end

      expect(result).to be_success
      expect(result.input).to eql(500)
      expect(Dry::Types::CallContext.current).to be_nil
    end

    it "restores the previous context when the block raises" do
      expect {
        Dry::Types::CallContext.with(table_a) { raise "boom" }
      }.to raise_error("boom")

      expect(Dry::Types::CallContext.current).to be_nil
    end
  end

  describe "context-aware constructors" do
    it "is supported by Constructor.[]" do
      type = Dry::Types::Constructor[Integer, context: true, fn: -> value, ctx { value * ctx }]

      expect(type.call_with_context(5, context: 3)).to eql(15)
    end

    it "is supported when appending to an existing constructor" do
      type = Dry::Types["integer"]
        .constructor { |value| value.to_i }
        .constructor(context: true) { |value, ctx| value * ctx.fetch("USD") }

      expect(type.call_with_context("5", context: table_a)).to eql(500)
      expect(type.call_with_context("5", context: table_b)).to eql(35)
    end

    it "is supported when prepending to an existing constructor" do
      type = Dry::Types["integer"]
        .constructor(context: true) { |value, ctx| value * ctx.fetch("USD") }
        .prepend(context: true) { |value, ctx| value + ctx.fetch("EUR") }

      expect(type.call_with_context(1, context: table_a)).to eql(20100)
    end

    it "refuses to attach a context to an already built function" do
      fn = Dry::Types::Constructor::Function[-> value { value }]

      expect {
        Dry::Types::Constructor::Function[fn, context: true]
      }.to raise_error(ArgumentError, /already built Function/)
    end

    it "marks context-aware functions" do
      type = Dry::Types["integer"].constructor(context: true) { |value, _ctx| value }

      expect(type.fn).to be_context
      expect(Dry::Types["integer"].constructor { |value| value }.fn).not_to be_context
    end
  end
end
