# frozen_string_literal: true

RSpec.describe Dry::Types::TaggedUnion do
  subject(:event) do
    Dry::Types::TaggedUnion.new(:kind, "create" => create_type, "cancel" => cancel_type)
  end

  let(:create_type) do
    Dry::Types["hash"].schema(
      kind: Dry::Types["strict.string"],
      id: Dry::Types["strict.integer"]
    )
  end

  let(:cancel_type) do
    Dry::Types["hash"].schema(
      kind: Dry::Types["strict.string"],
      reason: Dry::Types["strict.string"]
    )
  end

  it "is frozen" do
    expect(event).to be_frozen
  end

  describe "#call" do
    it "dispatches to the member selected by the tag" do
      expect(event.(kind: "create", id: 1)).to eql(kind: "create", id: 1)
      expect(event.(kind: "cancel", reason: "spam")).to eql(
        kind: "cancel", reason: "spam"
      )
    end

    it "is aliased as #[]" do
      expect(event[kind: "create", id: 1]).to eql(kind: "create", id: 1)
    end

    it "raises MissingTagError when the tag field is absent" do
      expect { event.(id: 1) }.to raise_error(
        Dry::Types::MissingTagError, ":kind tag is missing in Hash input"
      ) do |error|
        expect(error.tag).to be(:kind)
      end
    end

    it "raises UnknownTagError for an unregistered tag value" do
      expect { event.(kind: "update") }.to raise_error(
        Dry::Types::UnknownTagError,
        %q("update" is not a known :kind tag (known tags: "create", "cancel"))
      ) do |error|
        expect(error.tag).to be(:kind)
        expect(error.value).to eql("update")
      end
    end

    it "raises a CoercionError for non-hash input" do
      expect { event.(nil) }.to raise_error(
        Dry::Types::CoercionError, /must be a Hash to read :kind tag/
      )
    end

    it "propagates the selected member error without trying other members" do
      expect { event.(kind: "create", id: "not-an-int") }.to raise_error(
        Dry::Types::SchemaError
      ) do |error|
        expect(error.message).to include(":id")
      end
    end

    it "does not run constructors or defaults of non-selected members" do
      tracked = []

      create = create_type.constructor { |hash|
        tracked << :create_constructor
        hash
      }.schema(id: Dry::Types["strict.integer"].default {
        tracked << :create_default
        0
      })

      cancel = cancel_type.constructor { |hash|
        tracked << :cancel_constructor
        hash
      }.schema(reason: Dry::Types["strict.string"].default {
        tracked << :cancel_default
        "none"
      })

      union = Dry::Types::TaggedUnion.new(:kind, "create" => create, "cancel" => cancel)

      union.(kind: "create", id: 7)

      expect(tracked).to eql([:create_constructor])
      expect(tracked).not_to include(:cancel_constructor, :cancel_default)
    end
  end

  describe "tag lookup convention" do
    it "reads the symbol tag key first" do
      input = {kind: "create", "kind" => "cancel", id: 1}
      expect(event.(input)).to eql(kind: "create", id: 1)
    end

    it "falls back to the string tag key" do
      permissive = Dry::Types::TaggedUnion.new(
        :kind,
        "create" => Dry::Types["nominal.hash"],
        "cancel" => Dry::Types["nominal.hash"]
      )

      expect(permissive.({"kind" => "cancel", "reason" => "x"})).to eql(
        "kind" => "cancel", "reason" => "x"
      )
    end

    it "does not coerce string and symbol tag values" do
      symbol_members = Dry::Types::TaggedUnion.new(
        :kind,
        create: Dry::Types["nominal.hash"],
        cancel: Dry::Types["nominal.hash"]
      )
      string_members = Dry::Types::TaggedUnion.new(
        :kind,
        "create" => Dry::Types["nominal.hash"],
        "cancel" => Dry::Types["nominal.hash"]
      )

      expect(symbol_members.(kind: :create)).to eql(kind: :create)

      expect { string_members.(kind: :create) }.to raise_error(
        Dry::Types::UnknownTagError
      )
      expect { symbol_members.(kind: "create") }.to raise_error(
        Dry::Types::UnknownTagError
      )
    end
  end

  describe "#try" do
    it "returns a success" do
      result = event.try(kind: "create", id: 1)

      expect(result).to be_success
      expect(result.input).to eql(kind: "create", id: 1)
    end

    it "returns a failure for a missing tag without touching other members" do
      result = event.try(id: 1)

      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::MissingTagError)
    end

    it "returns a failure for an unknown tag" do
      result = event.try(kind: "update")

      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::UnknownTagError)
    end

    it "returns the selected member failure without falling through" do
      result = event.try(kind: "create", id: "bad")

      expect(result).to be_failure
      expect(result.error).to be_a(Dry::Types::CoercionError)
      expect(result.error.message).to include("doesn't conform schema")
    end

    it "yields the failure when a block is given" do
      yielded = nil
      result = event.try(kind: "update") { |failure| yielded = failure }

      expect(result).to be(yielded)
      expect(yielded.error).to be_a(Dry::Types::UnknownTagError)
    end
  end

  describe "#valid?" do
    it "is true for a value conforming the selected member" do
      expect(event).to be_valid(kind: "create", id: 1)
    end

    it "is false for a missing tag" do
      expect(event).not_to be_valid(id: 1)
    end

    it "is false for an unknown tag" do
      expect(event).not_to be_valid(kind: "update")
    end

    it "is false when the selected member rejects the value" do
      expect(event).not_to be_valid(kind: "create", id: "bad")
    end
  end

  describe "#call with a block" do
    it "calls the block on a missing tag" do
      expect(event.(id: 1) { :fallback }).to be(:fallback)
    end

    it "calls the block when the selected member fails" do
      expect(event.(kind: "create", id: "bad") { :fallback }).to be(:fallback)
    end

    it "does not call the block for a valid value" do
      expect(event.(kind: "create", id: 1) { :fallback }).to eql(
        kind: "create", id: 1
      )
    end
  end

  describe "#to_ast" do
    it "dumps a tagged_union node" do
      expect(event.to_ast).to eql(
        [:tagged_union, [
          :kind,
          [
            ["create", create_type.to_ast],
            ["cancel", cancel_type.to_ast]
          ],
          {}
        ]]
      )
    end

    it "dumps meta" do
      expect(event.meta(key: :value).to_ast).to eql(
        [:tagged_union, [
          :kind,
          [
            ["create", create_type.to_ast],
            ["cancel", cancel_type.to_ast]
          ],
          {key: :value}
        ]]
      )
    end
  end

  describe "AST compilation" do
    let(:compiler) { Dry::Types::Compiler.new(Dry::Types) }

    it "rebuilds an equal type" do
      expect(compiler.(event.to_ast)).to eql(event)
    end

    it "keeps dispatch results consistent" do
      rebuilt = compiler.(event.to_ast)

      expect(rebuilt.(kind: "create", id: 1)).to eql(event.(kind: "create", id: 1))
      expect { rebuilt.(kind: "nope") }.to raise_error(Dry::Types::UnknownTagError)
      expect { rebuilt.(id: 1) }.to raise_error(Dry::Types::MissingTagError)
    end

    it "rebuilds a type nested in an array member" do
      list = Dry::Types["nominal.array"].of(event)
      rebuilt = compiler.(list.to_ast)

      expect(rebuilt).to eql(list)
      expect(rebuilt.([{kind: "create", id: 1}])).to eql([{kind: "create", id: 1}])
    end

    it "rebuilds a type nested in a schema key" do
      schema = Dry::Types["hash"].schema(payload: event)
      rebuilt = compiler.(schema.to_ast)

      expect(rebuilt).to eql(schema)
      expect(rebuilt.(payload: {kind: "create", id: 1})).to eql(
        payload: {kind: "create", id: 1}
      )
    end
  end

  describe "factory method" do
    it "builds a type via Types() extension" do
      module TestTypes
        include Dry.Types()
      end

      type = TestTypes.TaggedUnion(:kind, "create" => TestTypes::Nominal::Hash)

      expect(type).to be_a(Dry::Types::TaggedUnion)
      expect(type.tag).to be(:kind)
    end

    it "accepts the mapping without curly braces" do
      type = Dry::Types::TaggedUnion.new(
        :kind,
        "create" => create_type,
        "cancel" => cancel_type
      )

      expect(type).to eql(event)
    end

    it "accepts the mapping as a positional hash" do
      type = Dry::Types::TaggedUnion.new(
        :kind,
        {"create" => create_type, "cancel" => cancel_type}
      )

      expect(type).to eql(event)
    end

    it "raises on a non-symbol tag" do
      expect {
        Dry::Types::TaggedUnion.new("kind", "create" => create_type)
      }.to raise_error(ArgumentError, /tag must be a Symbol/)
    end

    it "raises on an empty mapping" do
      expect {
        Dry::Types::TaggedUnion.new(:kind)
      }.to raise_error(ArgumentError, /mapping must be a non-empty Hash/)
    end
  end

  describe "#meta" do
    it "stores and rebuilds with metadata" do
      with_meta = event.meta(key: :value)

      expect(with_meta.meta).to eql(key: :value)
      expect(with_meta).not_to eql(event)
    end
  end
end
