# frozen_string_literal: true

require "spec_helper"

RSpec.describe Pundit::Decision do
  describe ".allow" do
    it "builds an allowed decision" do
      decision = described_class.allow

      expect(decision.allowed).to be(true)
      expect(decision).to be_allowed
    end

    it "preserves an optional code and details" do
      decision = described_class.allow(code: :ok, details: { note: "fine" })

      expect(decision.code).to eq(:ok)
      expect(decision.details).to eq({ note: "fine" })
    end
  end

  describe ".deny" do
    it "builds a denied decision" do
      decision = described_class.deny

      expect(decision.allowed).to be(false)
      expect(decision).not_to be_allowed
    end

    it "preserves an optional code and details" do
      decision = described_class.deny(code: :not_owner, details: { user_id: 42 })

      expect(decision.code).to eq(:not_owner)
      expect(decision.details).to eq({ user_id: 42 })
    end
  end

  describe "#allowed?" do
    it "coerces the raw flag to a boolean" do
      expect(described_class.new(:truthy)).to be_allowed
      expect(described_class.new(nil)).not_to be_allowed
    end
  end

  describe ".allowed?" do
    it "uses the decision's allowed state instead of its truthiness" do
      denied = described_class.deny(code: :blocked)

      expect(denied).to be_truthy
      expect(described_class.allowed?(denied)).to be(false)
    end

    it "accepts allowed decisions" do
      expect(described_class.allowed?(described_class.allow)).to be(true)
    end

    it "keeps plain boolean policy results working" do
      expect(described_class.allowed?(true)).to be(true)
      expect(described_class.allowed?(false)).to be(false)
      expect(described_class.allowed?(nil)).to be(false)
    end
  end
end
