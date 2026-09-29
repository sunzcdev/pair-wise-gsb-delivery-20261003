# frozen_string_literal: true

RSpec.describe Dry::Validation::Result, "warnings" do
  subject(:result) do
    contract.call(input)
  end

  let(:contract) do
    contract_class.new
  end

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      schema do
        required(:email).filled(:string)
        required(:legacy).filled(:string)
      end

      rule(:email) do
        key.failure("is invalid")
      end

      rule(:legacy) do
        key.warning(:deprecated)
      end
    end
  end

  let(:input) do
    {email: "foo", legacy: "bar"}
  end

  before do
    contract_class.config.messages.load_paths << SPEC_ROOT
      .join("fixtures/messages/errors.en.yml").realpath
  end

  describe "#warnings" do
    it "returns a MessageSet with warning messages" do
      expect(result.warnings).to be_a(Dry::Validation::MessageSet)
      expect(result.warnings.to_h).to eql(legacy: ["is deprecated, please migrate"])
    end

    it "marks every message as a warning" do
      expect(result.warnings.to_a).to all(be_warning)
    end

    it "supports full messages via options" do
      expect(result.warnings(full: true).to_h)
        .to eql(legacy: ["Legacy field is deprecated, please migrate"])
    end
  end

  describe "#success? and #failure?" do
    it "fails when errors are present regardless of warnings" do
      expect(result).to be_failure
      expect(result).not_to be_success
    end

    context "when only warnings are present" do
      let(:contract_class) do
        Class.new(Dry::Validation::Contract) do
          schema do
            required(:legacy).filled(:string)
          end

          rule(:legacy) do
            key.warning("is deprecated")
          end
        end
      end

      let(:input) do
        {legacy: "bar"}
      end

      it "is successful" do
        expect(result).to be_success
        expect(result.errors).to be_empty
        expect(result.warnings).not_to be_empty
      end
    end
  end

  describe "#warning?" do
    it "reports a warning for the warned key" do
      expect(result.warning?(:legacy)).to be(true)
    end

    it "does not report a warning for keys without warnings" do
      expect(result.warning?(:email)).to be(false)
    end
  end

  describe "#diagnostics" do
    it "contains both errors and warnings" do
      expect(result.diagnostics.to_h).to eql(
        email: ["is invalid"],
        legacy: ["is deprecated, please migrate"]
      )
    end

    it "does not change errors or warnings" do
      result.diagnostics

      expect(result.errors.to_h).to eql(email: ["is invalid"])
      expect(result.warnings.to_h).to eql(legacy: ["is deprecated, please migrate"])
    end

    it "exposes severity without parsing message text" do
      by_path = result.diagnostics.to_a.to_h { |msg| [msg.path, msg.warning?] }

      expect(by_path).to eql([:email] => false, [:legacy] => true)
    end

    context "with failures and warnings on the same path" do
      let(:contract_class) do
        Class.new(Dry::Validation::Contract) do
          schema do
            required(:email).filled(:string)
          end

          rule(:email) do
            key.warning("legacy format")
            key.failure("is invalid")
            key.warning("another deprecation")
          end
        end
      end

      let(:input) do
        {email: "foo"}
      end

      it "orders the failure before all warnings, keeping warning order" do
        expect(result.diagnostics[:email]).to eql(
          ["is invalid", "legacy format", "another deprecation"]
        )
      end

      it "reports severity alongside each diagnostic" do
        expect(result.diagnostics.to_a.map(&:warning?)).to eql([false, true, true])
      end
    end
  end

  describe "#inspect" do
    it "includes warnings when present" do
      expect(result.inspect).to include(
        'errors={email: ["is invalid"]}',
        'warnings={legacy: ["is deprecated, please migrate"]}'
      )
    end
  end
end
