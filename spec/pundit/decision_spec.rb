# frozen_string_literal: true

require "spec_helper"

RSpec.describe Pundit::Decision do
  describe ".allow" do
    it "builds an allowed decision" do
      decision = described_class.allow

      expect(decision).to be_allowed
      expect(decision).not_to be_denied
      expect(decision.code).to be_nil
      expect(decision.details).to be_nil
    end

    it "accepts a code and details" do
      decision = described_class.allow(code: :ok, details: {role: :admin})

      expect(decision.code).to eq(:ok)
      expect(decision.details).to eq({role: :admin})
    end
  end

  describe ".deny" do
    it "builds a denied decision" do
      decision = described_class.deny

      expect(decision).to be_denied
      expect(decision).not_to be_allowed
    end

    it "accepts a code and details" do
      decision = described_class.deny(code: :not_owner, details: {owner_id: 1})

      expect(decision.code).to eq(:not_owner)
      expect(decision.details).to eq({owner_id: 1})
    end
  end

  describe ".new" do
    it "coerces the allowed state to a boolean" do
      expect(described_class.new(allowed: nil)).to be_denied
      expect(described_class.new(allowed: "yes")).to be_allowed
    end

    it "is immutable" do
      expect(described_class.allow).to be_frozen
    end
  end

  describe "value equality" do
    it "equals a decision with the same state" do
      expect(described_class.deny(code: :not_owner, details: {a: 1}))
        .to eq(described_class.deny(code: :not_owner, details: {a: 1}))
    end

    it "does not equal a decision with a different state" do
      expect(described_class.deny(code: :not_owner)).not_to eq(described_class.deny(code: :other))
      expect(described_class.allow).not_to eq(described_class.deny)
    end

    it "hashes equal decisions equally" do
      expect(described_class.allow.hash).to eq(described_class.allow.hash)
    end
  end
end
