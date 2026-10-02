# frozen_string_literal: true

module RecursiveDefTypes
  include Dry.Types()
end

RSpec.describe Dry::Types::Recursive::Definitions do
  describe "closure checking" do
    it "raises when a referenced name never gets a body" do
      expect {
        Dry::Types::Recursive.define do |r|
          r[:a] = RecursiveDefTypes::Hash.schema(b: r[:b])
        end
      }.to raise_error(
        Dry::Types::Recursive::UnresolvedReferenceError,
        /not closed.*:b/
      )
    end

    it "lists all missing names" do
      begin
        Dry::Types::Recursive.define do |r|
          r[:a] = RecursiveDefTypes::Hash.schema(b: r[:b], c: r[:c])
        end
      rescue Dry::Types::Recursive::UnresolvedReferenceError => error
        expect(error.missing).to contain_exactly(:b, :c)
      else
        raise "expected UnresolvedReferenceError"
      end
    end

    it "does not require a reference for every defined name" do
      group = nil

      expect {
        group = Dry::Types::Recursive.define do |r|
          r[:used] = RecursiveDefTypes::Integer
          r[:unused] = RecursiveDefTypes::String
        end
      }.not_to raise_error

      expect(group[:used].(1)).to be(1)
    end

    it "rejects assigning the same name twice" do
      expect {
        Dry::Types::Recursive.define do |r|
          r[:a] = RecursiveDefTypes::Integer
          r[:a] = RecursiveDefTypes::String
        end
      }.to raise_error(Dry::Types::Recursive::RecursiveDefinitionError, /already defined/)
    end

    it "rejects a non-type body" do
      expect {
        Dry::Types::Recursive.define do |r|
          r[:a] = 42
        end
      }.to raise_error(::ArgumentError, /is not a Dry::Types type/)
    end

    it "rejects resolving before finalization" do
      definitions = described_class.new
      reference = definitions[:a]
      definitions[:a] = RecursiveDefTypes::Integer

      expect { reference.call(1) }.to raise_error(
        Dry::Types::Recursive::RecursiveDefinitionError, /not finalized/
      )
    end
  end

  describe "mutual recursion" do
    subject(:group) do
      Dry::Types::Recursive.define do |r|
        r[:foo] = RecursiveDefTypes::Hash.schema(bar?: r[:bar])
        r[:bar] = RecursiveDefTypes::Hash.schema(foo?: r[:foo])
      end
    end

    it "coerces through both types" do
      input = {bar: {foo: {}}}
      expect(group[:foo].(input)).to eql(input)
    end

    it "allows productive recursion through differently named references" do
      # pair -> item -> pair on a *different*, nested value must not be
      # mistaken for a cycle or a non-productive definition.
      input = {left: 1, right: {left: 2, right: 3}}
      expect(group_item[:pair].(input)).to eql(input)
    end

    let(:group_item) do
      Dry::Types::Recursive.define do |r|
        r[:pair] = RecursiveDefTypes::Hash.schema(left: r[:item], right: r[:item])
        r[:item] = RecursiveDefTypes::Integer | r[:pair]
      end
    end

    it "detects a cycle entering through foo" do
      input = {bar: {}}
      input[:bar][:foo] = input

      expect { group[:foo].(input) }.to raise_error(
        Dry::Types::Recursive::CyclicInputError
      ) { |error|
        expect(error.path).to eql([:bar, :foo])
      }
    end

    it "detects a cycle entering through bar" do
      input = {foo: {}}
      input[:foo][:bar] = input

      expect { group[:bar].(input) }.to raise_error(
        Dry::Types::Recursive::CyclicInputError
      ) { |error|
        expect(error.path).to eql([:foo, :bar])
      }
    end

    it "supports instance_eval blocks" do
      group = Dry::Types::Recursive.define do
        define(:x, RecursiveDefTypes::Hash.schema(y?: self[:y]))
        define(:y, RecursiveDefTypes::Hash.schema(x?: self[:x]))
      end

      expect(group[:x].(y: {})).to eql(y: {})
    end

    it "supports bare method references in instance_eval blocks" do
      group = Dry::Types::Recursive.define do
        define(:x, RecursiveDefTypes::Hash.schema(y?: y))
        define(:y, RecursiveDefTypes::Hash.schema(x?: x))
      end

      expect(group[:x].(y: {})).to eql(y: {})
    end

    it "detects a cycle that goes through differently named references" do
      cyclic_group = Dry::Types::Recursive.define do |r|
        r[:foo] = RecursiveDefTypes::Hash.schema(bar?: r[:bar])
        r[:bar] = RecursiveDefTypes::Hash.schema(foo?: r[:foo])
      end

      a = {bar: {}}
      a[:bar][:foo] = a

      expect { cyclic_group[:foo].(a) }.to raise_error(
        Dry::Types::Recursive::CyclicInputError
      ) { |error|
        expect(error.path).to eql([:bar, :foo])
      }
    end
  end

  describe "[] access" do
    it "memoizes references by name" do
      definitions = described_class.new
      expect(definitions[:a]).to be(definitions[:a])
    end
  end
end