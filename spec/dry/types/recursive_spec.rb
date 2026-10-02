# frozen_string_literal: true

RSpec.describe Dry::Types::Recursive do
  # An approval condition tree: leaves are comparisons, branches
  # combine child conditions with "and"/"or"
  let(:scope) do
    Dry::Types.recursive do |t|
      t.define(:leaf) do
        Dry::Types["nominal.hash"].schema(
          op: Dry::Types["strict.string"],
          value: Dry::Types["strict.integer"]
        )
      end

      t.define(:node) do
        Dry::Types["nominal.hash"].schema(
          op: Dry::Types["strict.string"].enum("and", "or"),
          children: Dry::Types["nominal.array"].of(t.ref(:condition))
        )
      end

      t.define(:condition) do
        t.ref(:leaf) | t.ref(:node)
      end
    end
  end

  subject(:condition) { scope[:condition] }

  let(:valid_input) do
    {
      op: "and",
      children: [
        {op: "eq", value: 1},
        {op: "or", children: [{op: "gt", value: 2}]}
      ]
    }
  end

  it "coerces valid trees" do
    expect(condition.(valid_input)).to eql(valid_input)
  end

  it "supports mutually recursive definitions" do
    expect(scope[:node].(op: "or", children: [{op: "eq", value: 1}]))
      .to eql(op: "or", children: [{op: "eq", value: 1}])
  end

  it "rejects invalid leaves" do
    expect {
      condition.(op: "and", children: [{op: "eq", value: "1"}])
    }.to raise_error(Dry::Types::CoercionError)
  end

  it "coerces deeply nested trees with stable termination" do
    input = 100.times.reduce({op: "eq", value: 1}) { |acc, _|
      {op: "and", children: [acc]}
    }

    expect(condition.(input)).to eql(input)
  end

  it "accepts inputs sharing substructures without cycles" do
    shared = {op: "eq", value: 1}
    input = {op: "and", children: [shared, shared]}

    expect(condition.(input)).to eql(input)
  end

  context "with self-referential input" do
    let(:cyclic) do
      {op: "and", children: []}.tap { |input| input[:children] << input }
    end

    it "raises RecursiveInputError pointing at the self-reference" do
      expect { condition.(cyclic) }.to raise_error(Dry::Types::RecursiveInputError) do |error|
        expect(error.path).to eql([:children, 0])
        expect(error.first_seen_path).to eql([])
        expect(error.message).to include("children[0]")
      end
    end

    it "reports the position of nested self-references" do
      root = {op: "and", children: []}
      child = {op: "or", children: [{op: "eq", value: 1}, root]}
      root[:children] << child

      expect { condition.(root) }.to raise_error(Dry::Types::RecursiveInputError) do |error|
        expect(error.path).to eql([:children, 0, :children, 1])
        expect(error.first_seen_path).to eql([])
      end
    end

    it "returns false from #valid?" do
      expect(condition.valid?(cyclic)).to be(false)
    end

    it "returns a failure from #try" do
      expect(condition.try(cyclic)).to be_failure
    end
  end

  describe "definition" do
    it "rejects scopes with unresolved references" do
      expect {
        Dry::Types.recursive do |t|
          t.define(:node) do
            Dry::Types["nominal.hash"].schema(
              children: Dry::Types["nominal.array"].of(t.ref(:missing))
            )
          end
        end
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /:missing/)
    end

    it "rejects definitions that recurse without consuming input" do
      expect {
        Dry::Types.recursive do |t|
          t.define(:a) { t.ref(:a) | Dry::Types["strict.integer"] }
        end
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /never consumes input/)
    end

    it "rejects mutually recursive definitions that never consume input" do
      expect {
        Dry::Types.recursive do |t|
          t.define(:a) { t.ref(:b) }
          t.define(:b) { t.ref(:a) | Dry::Types["strict.integer"] }
        end
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /never consumes input/)
    end

    it "rejects duplicate definitions" do
      expect {
        Dry::Types.recursive do |t|
          t.define(:a) { Dry::Types["strict.integer"] }
          t.define(:a) { Dry::Types["strict.string"] }
        end
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /already defined/)
    end

    it "rejects non-type definitions" do
      expect {
        Dry::Types.recursive do |t|
          t.define(:a) { 42 }
        end
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /must be a Dry::Types::Type/)
    end

    it "rejects new definitions after finalization" do
      finalized = Dry::Types.recursive do |t|
        t.define(:a) { Dry::Types["strict.integer"] }
      end

      expect {
        finalized.define(:b) { Dry::Types["strict.string"] }
      }.to raise_error(Dry::Types::RecursiveDefinitionError, /finalized/)
    end
  end

  describe "#to_ast" do
    it "exports references by name" do
      expect(scope.ref(:condition).to_ast).to eql([:recursive, [:condition, {}]])
    end

    it "exports the whole scope as a finite definitions node" do
      name, (definitions,) = scope.to_ast

      expect(name).to be(:recursive_definitions)
      expect(definitions.map(&:first)).to eql(%i[leaf node condition])
    end

    it "rebuilds an equivalent scope from the exported AST" do
      ast = scope.to_ast
      rebuilt = Dry::Types::Compiler.new(Dry::Types).(ast)

      expect(rebuilt.to_ast).to eql(ast)
      expect(rebuilt[:condition].(valid_input)).to eql(valid_input)
    end

    it "rejects recursive references compiled without definitions" do
      expect {
        Dry::Types::Compiler.new(Dry::Types).([:recursive, [:condition, {}]])
      }.to raise_error(ArgumentError, /recursive/i)
    end
  end

  describe "#inspect" do
    it "prints references by name without expanding them" do
      expect(condition.inspect).to eql(
        "#<Dry::Types[Sum<Recursive<:leaf> | Recursive<:node>>]>"
      )
    end
  end
end
