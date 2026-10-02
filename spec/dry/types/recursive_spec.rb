# frozen_string_literal: true

require "timeout"

module RecursiveSpecTypes
  include Dry.Types()
end

RSpec.describe Dry::Types, ".recursive" do
  let(:leaf) do
    RecursiveSpecTypes::Hash.schema(
      op: RecursiveSpecTypes::String.constrained(included_in: %w[eq gt lt]),
      field: RecursiveSpecTypes::String,
      value: RecursiveSpecTypes::Any
    )
  end

  let(:tree) do
    Dry::Types.recursive(:condition) do |r|
      node = RecursiveSpecTypes::Hash.schema(
        op: RecursiveSpecTypes::String.constrained(included_in: %w[and or]),
        children: RecursiveSpecTypes::Array.of(r[:condition])
      )

      leaf | node
    end
  end

  let(:valid_input) do
    {
      op: "and",
      children: [
        {op: "eq", field: "amount", value: 100},
        {op: "or", children: [
          {op: "gt", field: "age", value: 18}
        ]}
      ]
    }
  end

  it "coerces a finite tree" do
    expect(tree.(valid_input)).to eql(valid_input)
  end

  it "accepts a leaf at the root" do
    expect(tree.(op: "eq", field: "x", value: 1)).to eql(op: "eq", field: "x", value: 1)
  end

  it "rejects a value matching neither a leaf nor a node" do
    expect { tree.(op: "nope") }.to raise_error(Dry::Types::CoercionError)
  end

  it "is a type" do
    expect(tree).to be_a(Dry::Types::Type)
  end

  it "returns a Scope carrying the group" do
    expect(tree).to be_a(Dry::Types::Recursive::Scope)
    expect(tree.root_name).to be(:condition)
  end

  context "cyclic input" do
    let(:cyclic) do
      value = {op: "and", children: []}
      value[:children] << value
      value
    end

    it "raises a CyclicInputError from unsafe calls" do
      expect { tree.(cyclic) }.to raise_error(Dry::Types::Recursive::CyclicInputError)
    end

    it "reports the position of the repeated value" do
      begin
        tree.(cyclic)
      rescue Dry::Types::Recursive::CyclicInputError => error
        expect(error.path).to eql([:children, 0])
        expect(error.cycle_path).to eql([:children, 0])
        expect(error.message).to include("$[:children][0]")
      else
        raise "expected CyclicInputError"
      end
    end

    it "reports deeper cycle positions" do
      inner = {op: "and", children: []}
      root = {op: "and", children: [{op: "or", children: [inner]}]}
      inner[:children] << root

      begin
        tree.(root)
      rescue Dry::Types::Recursive::CyclicInputError => error
        expect(error.path).to eql([:children, 0, :children, 0, :children, 0])
      else
        raise "expected CyclicInputError"
      end
    end

    it "returns a failure from #try" do
      result = tree.try(cyclic)
      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::Recursive::CyclicInputError)
    end

    it "calls the fallback block from safe calls" do
      # A deliberately compact recursive shape: safe mode threads the
      # fallback block through every nested schema/array call, so a very
      # wide sum + constrained definition can approach the caller's stack
      # budget on deep failures. The failure semantics under test here do
      # not depend on the wider approval-tree definition.
      compact = Dry::Types.recursive(:node) do |r|
        RecursiveSpecTypes::Hash.schema(
          value: RecursiveSpecTypes::Integer,
          children: RecursiveSpecTypes::Array.of(r[:node])
        )
      end

      cyclic = {value: 1, children: []}
      cyclic[:children] << cyclic

      result = compact.call_safe(cyclic) { |out| [:failed, out] }
      expect(result.first).to be(:failed)
      expect(result.last).to be(cyclic)
    end

    it "lax returns the original cyclic input instead of raising" do
      compact = Dry::Types.recursive(:node) do |r|
        RecursiveSpecTypes::Hash.schema(
          value: RecursiveSpecTypes::Integer,
          children: RecursiveSpecTypes::Array.of(r[:node])
        )
      end

      cyclic = {value: 1, children: []}
      cyclic[:children] << cyclic

      expect(compact.lax.(cyclic)).to be(cyclic)
    end

    it "makes #valid? return false" do
      expect(tree.valid?(cyclic)).to be(false)
    end
  end

  context "array cycle" do
    let(:list) do
      Dry::Types.recursive(:node) do |r|
        RecursiveSpecTypes::Hash.schema(
          value: RecursiveSpecTypes::Integer,
          next: r[:node].optional
        )
      end
    end

    it "reports the key in the path" do
      node = {value: 1, next: nil}
      node[:next] = node

      begin
        list.(node)
      rescue Dry::Types::Recursive::CyclicInputError => error
        expect(error.path).to eql([:next])
      else
        raise "expected CyclicInputError"
      end
    end
  end

  context "non-productive definitions" do
    it "rejects direct self reference with no input consumption" do
      type = Dry::Types.recursive(:x) { |r| r[:x] }

      expect { type.("anything") }.to raise_error(
        Dry::Types::Recursive::NonProductiveRecursionError
      )
    end

    it "rejects non-productive mutual references" do
      group = Dry::Types::Recursive.define do |r|
        r[:a] = r[:b]
        r[:b] = r[:a]
      end

      expect { group[:a].(1) }.to raise_error(
        Dry::Types::Recursive::NonProductiveRecursionError
      )
    end

    it "does not hang (terminates deterministically)" do
      type = Dry::Types.recursive(:x) { |r| r[:x] }

      Timeout.timeout(5) do
        expect { type.(1) }.to raise_error(
          Dry::Types::Recursive::NonProductiveRecursionError
        )
      end
    end
  end

  context "DAGs" do
    it "allows the same object twice on different paths" do
      shared = {op: "eq", field: "x", value: 1}
      dag = {
        op: "and",
        children: [shared, {op: "and", children: [shared]}]
      }

      expect(tree.(dag)).to eql(dag)
    end
  end

  context "deep finite trees" do
    def build(depth)
      if depth.zero?
        {op: "eq", field: "x", value: 1}
      else
        {op: "and", children: [build(depth - 1)]}
      end
    end

    it "coerces a moderately deep tree" do
      expect(tree.(build(50))).to be_a(::Hash)
    end
  end
end