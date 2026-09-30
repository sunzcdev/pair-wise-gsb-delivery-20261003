# frozen_string_literal: true

RSpec.describe Dry::Types::Union do
  before { stub_const("Types", Dry.Types()) }

  subject(:type) { Types.Union(:type, mapping) }

  it_behaves_like "Dry::Types::Nominal without primitive"
  it_behaves_like "Dry::Types::Nominal#meta"

  let(:create_type) do
    Types::Hash.schema(
      type: Types::String.constrained(eql: "create"),
      order_id: Types::Coercible::Integer
    )
  end

  let(:cancel_type) do
    Types::Hash.schema(
      type: Types::String.constrained(eql: "cancel"),
      reason: Types::String
    )
  end

  let(:mapping) { {"create" => create_type, "cancel" => cancel_type} }

  describe "construction" do
    it "is built through the Types.Union DSL" do
      expect(type).to be_a(described_class)
      expect(type.tag).to be(:type)
      expect(type.mapping).to eql(mapping)
    end

    it "is frozen" do
      expect(type).to be_frozen
      expect(type.mapping).to be_frozen
    end

    it "lists branches in declaration order" do
      expect(type.branches).to eql([create_type, cancel_type])
    end

    it "rejects an empty mapping" do
      expect { described_class.new({}, tag: :type) }.to raise_error(
        ArgumentError, /non-empty hash/
      )
    end

    it "rejects a branch that is not a type" do
      expect { described_class.new({"create" => Integer}, tag: :type) }.to raise_error(
        ArgumentError, /must be a Dry::Types::Type/
      )
    end

    it "coerces a string tag name into a symbol" do
      union = described_class.new(mapping, tag: "type")
      expect(union.tag).to be(:type)
    end
  end

  describe "#call (unsafe)" do
    it "dispatches symbol-keyed input to the matching branch" do
      expect(type.(type: "create", order_id: "42")).to eql(
        type: "create", order_id: 42
      )
      expect(type.(type: "cancel", reason: "user request")).to eql(
        type: "cancel", reason: "user request"
      )
    end

    it "dispatches string-keyed input to the same branch" do
      symbolized = Types.Union(
        :type,
        "create" => create_type.with_key_transform(&:to_sym),
        "cancel" => cancel_type.with_key_transform(&:to_sym)
      )

      expect(symbolized.("type" => "create", "order_id" => "42")).to eql(
        type: "create", order_id: 42
      )
    end

    it "prefers the symbol tag when both key forms are present" do
      input = {type: "cancel", "type" => "create", reason: "ok"}
      expect(type.(input)).to eql(type: "cancel", reason: "ok")
    end

    it "matches tag values literally" do
      symbol_create = Types::Hash.schema(
        type: Types::Symbol.constrained(eql: :create),
        order_id: Types::Coercible::Integer
      )
      union = Types.Union(:type, :create => symbol_create)
      expect(union.(type: :create, order_id: 1)).to eql(type: :create, order_id: 1)
    end
  end

  describe "tag errors" do
    it "raises MissingUnionTagError when the tag field is absent" do
      expect { type.(order_id: 1) }.to raise_error(
        Dry::Types::MissingUnionTagError, /:type/
      ) { |error| expect(error.tag).to be(:type) }
    end

    it "raises MissingUnionTagError for an empty hash" do
      expect { type.({}) }.to raise_error(Dry::Types::MissingUnionTagError)
    end

    it "raises UnknownUnionTagError for an undeclared tag value" do
      expect { type.(type: "pause") }.to raise_error(
        Dry::Types::UnknownUnionTagError
      ) do |error|
        expect(error.tag).to be(:type)
        expect(error.value).to eql("pause")
      end
    end

    it "raises InvalidUnionInputError for non-hash input" do
      expect { type.("create") }.to raise_error(Dry::Types::InvalidUnionInputError) { |error|
        expect(error.tag).to be(:type)
      }
      expect { type.(nil) }.to raise_error(Dry::Types::InvalidUnionInputError)
      expect { type.(42) }.to raise_error(Dry::Types::InvalidUnionInputError)
    end

    it "has distinguishable tag and branch errors" do
      expect { type.({}) }.to raise_error(Dry::Types::UnionTagError)
      expect { type.(type: "create", order_id: "oops") }.to raise_error(Dry::Types::CoercionError)

      begin
        type.(type: "create", order_id: "oops")
      rescue Dry::Types::CoercionError => branch_error
        expect(branch_error).not_to be_a(Dry::Types::UnionTagError)
      end
    end
  end

  describe "branch isolation" do
    let(:create_calls) { [] }
    let(:cancel_calls) { [] }

    let(:tracked_create) do
      calls = create_calls
      create_type.constructor { |input, t| calls << :run; t.call(input) }
    end

    let(:tracked_cancel) do
      calls = cancel_calls
      cancel_type.constructor { |input, t| calls << :run; t.call(input) }
    end

    let(:tracked) { Types.Union(:type, "create" => tracked_create, "cancel" => tracked_cancel) }

    it "does not fall through to another branch when the selected branch fails" do
      expect { tracked.(type: "create", order_id: "not-an-int") }.to raise_error(
        Dry::Types::SchemaError
      )
      expect(create_calls).to eql([:run])
      expect(cancel_calls).to be_empty
    end

    it "never runs another branch constructor for an unknown tag" do
      expect { tracked.(type: "pause") }.to raise_error(Dry::Types::UnknownUnionTagError)
      expect(create_calls).to be_empty
      expect(cancel_calls).to be_empty
    end

    it "never runs any branch constructor for a missing tag" do
      expect { tracked.({}) }.to raise_error(Dry::Types::MissingUnionTagError)
      expect(create_calls).to be_empty
      expect(cancel_calls).to be_empty
    end

    it "evaluates a default only for the selected branch" do
      with_default = create_type.schema(note: Types::String.default("none".freeze))
      union = Types.Union(:type, "create" => with_default, "cancel" => cancel_type)

      expect(union.(type: "create", order_id: 1)).to eql(
        type: "create", order_id: 1, note: "none"
      )
      expect { union.(type: "cancel", reason: "x") }.not_to raise_error
    end
  end

  describe "#call with a block (safe mode)" do
    it "returns the branch result on success" do
      result = type.call(type: "create", order_id: "1") { :fallback }
      expect(result).to eql(type: "create", order_id: 1)
    end

    it "falls back for a missing tag" do
      expect(type.call({}) { :fallback }).to be(:fallback)
    end

    it "falls back for an unknown tag" do
      expect(type.call(type: "pause") { :fallback }).to be(:fallback)
    end

    it "falls back when the selected branch fails" do
      expect(type.call(type: "create", order_id: "bad") { :fallback }).to be(:fallback)
    end
  end

  describe "#try" do
    it "returns a success for a matching input" do
      result = type.try(type: "create", order_id: "1")
      expect(result).to be_success
      expect(result.input).to eql(type: "create", order_id: 1)
    end

    it "returns a failure carrying the missing tag error" do
      result = type.try({})
      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::MissingUnionTagError)
    end

    it "returns a failure carrying the unknown tag error" do
      result = type.try(type: "nope")
      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::UnknownUnionTagError)
    end

    it "returns the branch failure without trying other branches" do
      result = type.try(type: "create", order_id: "bad")
      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::CoercionError)
      expect(result.error).not_to be_a(Dry::Types::UnionTagError)
    end

    it "yields the failure when a block is given" do
      yielded = nil
      type.try({}) { |failure| yielded = failure }
      expect(yielded.error).to be_a(Dry::Types::MissingUnionTagError)
    end
  end

  describe "#valid? and #===" do
    it "is valid for input matching any branch" do
      expect(type).to be_valid(type: "create", order_id: 1)
      expect(type).to be_valid(type: "cancel", reason: "x")
    end

    it "is invalid for tag errors and branch failures" do
      expect(type).not_to be_valid({})
      expect(type).not_to be_valid(type: "pause")
      expect(type).not_to be_valid(type: "create", order_id: "bad")
    end

    it "is usable in a case statement" do
      expect(type === {type: "create", order_id: 1}).to be(true)
      expect(type === {}).to be(false)
    end
  end

  describe "#lax" do
    subject(:lax) { type.lax }

    it "keeps dispatching by the tag" do
      expect(lax.(type: "create", order_id: "1")).to eql(type: "create", order_id: 1)
    end

    it "returns the original input when a branch fails" do
      expect(lax.(type: "create", order_id: "bad")).to eql(type: "create", order_id: "bad")
    end

    it "still raises for a missing or unknown tag" do
      expect { lax.({}) }.to raise_error(Dry::Types::MissingUnionTagError)
      expect { lax.(type: "pause") }.to raise_error(Dry::Types::UnknownUnionTagError)
    end
  end

  describe "#meta" do
    it "carries metadata and rebuilds an equal type" do
      with_meta = type.meta(foo: :bar)
      expect(with_meta.meta).to eql(foo: :bar)
      expect(with_meta).not_to eql(type)
      expect(with_meta.(type: "cancel", reason: "x")).to eql(type: "cancel", reason: "x")
    end
  end

  describe "key transform ordering" do
    let(:symbolizing_create) { create_type.with_key_transform(&:to_sym) }
    let(:symbolizing_cancel) { cancel_type.with_key_transform(&:to_sym) }

    let(:union) do
      Types.Union(:type, "create" => symbolizing_create, "cancel" => symbolizing_cancel)
    end

    it "reads the tag before the branch transforms keys" do
      expect(union.("type" => "create", "order_id" => "7")).to eql(
        type: "create", order_id: 7
      )
    end

    it "handles symbol keys identically" do
      expect(union.(type: "cancel", reason: "ok")).to eql(type: "cancel", reason: "ok")
    end

    it "lets an outer constructor normalize keys before the tag is read" do
      wrapped = union.constructor { |input, t| t.call(input.transform_keys(&:to_sym)) }
      expect(wrapped.("type" => "create", "order_id" => "3")).to eql(
        type: "create", order_id: 3
      )
    end
  end

  describe "composition" do
    it "works as an array member" do
      list = Types::Array(type)
      input = [{type: "create", order_id: "1"}, {type: "cancel", reason: "x"}]
      expect(list.(input)).to eql(
        [{type: "create", order_id: 1}, {type: "cancel", reason: "x"}]
      )
    end

    it "works inside a schema" do
      schema = Types::Hash.schema(command: type)
      result = schema.(command: {type: "create", order_id: "5"})
      expect(result).to eql(command: {type: "create", order_id: 5})
    end

    it "surfaces tag errors from within an array" do
      list = Types::Array(type)
      expect { list.([{}]) }.to raise_error(Dry::Types::MissingUnionTagError)
      expect { list.([{type: "pause"}]) }.to raise_error(Dry::Types::UnknownUnionTagError)
    end
  end

  describe "AST" do
    let(:compiler) { Dry::Types::Compiler.new(Dry::Types) }

    it "exposes a :union AST node" do
      ast = type.to_ast
      expect(ast.first).to be(:union)
      expect(ast[1][0]).to be(:type)
      expect(ast[1][1].map(&:first)).to eql(["create", "cancel"])
    end

    it "round-trips and keeps dispatching the same way" do
      rebuilt = compiler.(type.to_ast)
      expect(rebuilt).to eql(type)

      input = {type: "create", order_id: "9"}
      expect(rebuilt.(input)).to eql(type.(input))
      expect { rebuilt.({}) }.to raise_error(Dry::Types::MissingUnionTagError)
      expect { rebuilt.(type: "pause") }.to raise_error(Dry::Types::UnknownUnionTagError)
    end

    it "round-trips when nested in an array and a schema" do
      schema = Types::Hash.schema(commands: Types::Array(type))
      rebuilt = compiler.(schema.to_ast)

      expect(rebuilt.to_ast).to eql(schema.to_ast)

      input = {commands: [{type: "create", order_id: "2"}, {type: "cancel", reason: "r"}]}
      expect(rebuilt.(input)).to eql(schema.(input))
    end

    it "round-trips metadata" do
      rebuilt = compiler.(type.meta(custom: true).to_ast)
      expect(rebuilt.meta).to eql(custom: true)
    end
  end

  describe "#inspect" do
    it "prints the tag and branches" do
      expect(type.inspect).to start_with("#<Dry::Types[Union<:type {")
      expect(type.inspect).to include('"create"')
      expect(type.inspect).to include('"cancel"')
    end
  end

  describe "inference" do
    it "infers hash predicates" do
      predicates = Dry::Types::PredicateInferrer.new[type]
      expect(predicates).to eql([:hash?])
    end

    it "infers primitives from the branches" do
      primitives = Dry::Types::PrimitiveInferrer.new[type]
      expect(primitives).to eql([Hash])
    end
  end
end
