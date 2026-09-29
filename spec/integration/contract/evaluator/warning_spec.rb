# frozen_string_literal: true

RSpec.describe Dry::Validation::Evaluator, "warnings" do
  subject(:contract) { contract_class.new }

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      schema do
        required(:email).filled(:string)
      end
    end
  end

  context "setting key warnings using default rule path" do
    before do
      contract_class.rule(:email) do
        key.warning("looks like a legacy format") if value.end_with?(".old")
      end
    end

    it "keeps the result successful" do
      result = contract.(email: "jane@doe.old")

      expect(result).to be_success
      expect(result).not_to be_failure
      expect(result.errors).to be_empty
    end

    it "exposes warnings via #warnings without mixing them into errors" do
      result = contract.(email: "jane@doe.old")

      expect(result.warnings.to_h).to eql(email: ["looks like a legacy format"])
      expect(result.errors.to_h).to eql({})
    end

    it "exposes all diagnostics via #diagnostics" do
      result = contract.(email: "jane@doe.old")

      expect(result.diagnostics.map(&:text)).to eql(["looks like a legacy format"])
      expect(result.diagnostics.map(&:path)).to eql([[:email]])
    end

    it "exposes severity on messages so callers don't parse message texts" do
      message = contract.(email: "jane@doe.old").diagnostics.first

      expect(message.severity).to be(:warning)
      expect(message).to be_warning
      expect(message).not_to be_failure
    end
  end

  context "setting base warnings" do
    before do
      contract_class.rule(:email) do
        base.warning("legacy input format is deprecated")
      end
    end

    it "sets warning under the base path" do
      result = contract.(email: "jane@doe.org")

      expect(result).to be_success
      expect(result.warnings.to_h).to eql(nil => ["legacy input format is deprecated"])
    end
  end

  context "setting warnings with meta data" do
    before do
      contract_class.rule(:email) do
        key.warning(text: "looks like a legacy format", code: "W001")
      end
    end

    it "sets warning with meta under specified key" do
      warnings = contract.(email: "jane@doe.org").warnings

      expect(warnings.to_h).to eql(email: [text: "looks like a legacy format", code: "W001"])
      expect(warnings.first.meta).to eql(code: "W001")
    end
  end

  context "setting warnings with localized messages" do
    before do
      contract_class.config.messages.load_paths << SPEC_ROOT
        .join("fixtures/messages/errors.en.yml").realpath

      contract_class.rule(:email) do
        key(:contact).warning(:wrong)
      end
    end

    it "resolves the message and keeps it a warning" do
      result = contract.(email: "jane@doe.org")

      expect(result).to be_success
      expect(result.warnings.to_h).to eql(contact: ["not right"])
      expect(result.warnings.first).to be_warning
    end
  end

  context "when localized message id is missing" do
    before do
      contract_class.rule(:email) do
        key.warning(:oops_bad_id)
      end
    end

    it "raises a meaningful error just like for failures" do
      expect { contract.(email: "jane@doe.org") }.to raise_error(
        Dry::Validation::MissingMessageError, /oops_bad_id/
      )
    end
  end

  context "when the same path has both a failure and a warning" do
    before do
      contract_class.rule(:email) do
        key.warning("looks like a legacy format") if value.end_with?(".old")
        key.failure("is not allowed") if value.start_with?("bad")
      end
    end

    it "keeps them in separate sets and makes the result a failure" do
      result = contract.(email: "bad@doe.old")

      expect(result).to be_failure
      expect(result.errors.to_h).to eql(email: ["is not allowed"])
      expect(result.warnings.to_h).to eql(email: ["looks like a legacy format"])
    end

    it "orders failures before warnings under the same path in diagnostics" do
      result = contract.(email: "bad@doe.old")

      expect(result.diagnostics.map(&:severity)).to eql(%i[failure warning])
      expect(result.diagnostics.map(&:text)).to eql(
        ["is not allowed", "looks like a legacy format"]
      )
    end
  end

  context "with dependent rules" do
    before do
      contract_class.rule(:email) do
        key.warning("looks like a legacy format") if value.end_with?(".old")
      end

      contract_class.rule(:email) do
        key.failure("is on a denylist") if value.start_with?("bad")
      end
    end

    it "does not skip dependent rules when only a warning was added" do
      result = contract.(email: "bad@doe.old")

      expect(result.errors.to_h).to eql(email: ["is on a denylist"])
      expect(result.warnings.to_h).to eql(email: ["looks like a legacy format"])
    end
  end

  context "with nested objects in an array" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        params do
          required(:items).array(:hash) do
            required(:name).filled(:string)
          end
        end

        rule(:items) do
          values[:items].each_with_index do |item, index|
            if item[:name].start_with?("old_")
              key([:items, index, :name]).warning("uses a legacy name")
            end
          end
        end
      end
    end

    it "sets warnings under nested paths within array items" do
      result = contract.(items: [{name: "old_jane"}, {name: "jane"}])

      expect(result).to be_success
      expect(result.errors).to be_empty
      expect(result.warnings.to_h).to eql(items: {0 => {name: ["uses a legacy name"]}})
      expect(result.diagnostics.map(&:path)).to eql([[:items, 0, :name]])
      expect(result.diagnostics.first).to be_warning
    end
  end
end
