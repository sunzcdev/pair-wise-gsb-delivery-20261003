# frozen_string_literal: true

RSpec.describe Dry::Transaction::StepFailure do
  def step(name)
    instance_double(Dry::Transaction::Step, name: name)
  end

  describe ".call" do
    it "wraps a plain value with a single-segment path" do
      failure = described_class.call(step(:validate), "nope") {}

      expect(failure.path).to eq [:validate]
      expect(failure.step.name).to eq :validate
      expect(failure.value).to eq "nope"
    end

    it "prepends the parent step to an already wrapped failure's path" do
      inner = described_class.call(step(:validate), "nope") {}
      outer = described_class.call(step(:create_user), inner) {}

      expect(outer.path).to eq [:create_user, :validate]
      expect(outer.step.name).to eq :validate
      expect(outer.value).to eq "nope"
    end

    it "preserves the original exception across multiple wrappings" do
      exception = Class.new(StandardError).new("email required")
      inner = described_class.call(step(:validate), exception) {}
      middle = described_class.call(step(:payment), inner) {}
      outer = described_class.call(step(:checkout), middle) {}

      expect(outer.path).to eq [:checkout, :payment, :validate]
      expect(outer.value).to be exception
    end

    it "yields for both new and already wrapped failures" do
      yielded = []

      inner = described_class.call(step(:validate), "nope") { yielded << :validate }
      described_class.call(step(:create_user), inner) { yielded << :create_user }

      expect(yielded).to eq %i[validate create_user]
    end
  end

  describe "#matches?" do
    subject(:failure) do
      described_class.new(step(:validate), "nope", path: %i[create_user validate])
    end

    it "matches the leaf step name" do
      expect(failure.matches?(:validate)).to be true
    end

    it "does not match a parent step name given as a plain name" do
      expect(failure.matches?(:create_user)).to be false
    end

    it "matches the full address" do
      expect(failure.matches?(%i[create_user validate])).to be true
    end

    it "matches a parent-level address prefix" do
      expect(failure.matches?([:create_user])).to be true
    end

    it "does not match other addresses" do
      expect(failure.matches?(%i[update_order validate])).to be false
      expect(failure.matches?([:validate])).to be false
      expect(failure.matches?(:other)).to be false
    end
  end
end
