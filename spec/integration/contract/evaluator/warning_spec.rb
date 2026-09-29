# frozen_string_literal: true

RSpec.describe Dry::Validation::Evaluator, "warnings" do
  subject(:contract) do
    contract_class.new
  end

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      schema do
        required(:email).filled(:string)
        optional(:name).filled(:string)
        optional(:items).array(:hash) do
          required(:kind).filled(:string)
        end
      end
    end
  end

  context "setting key warnings using default rule path" do
    before do
      contract_class.rule(:email) do
        key.warning("uses a deprecated format")
      end
    end

    it "keeps the result successful" do
      expect(contract.(email: "foo")).to be_success
    end

    it "does not mix warnings into errors" do
      result = contract.(email: "foo")

      expect(result.errors).to be_empty
      expect(result.errors.to_h).to eql({})
    end

    it "sets the warning under the rule key" do
      result = contract.(email: "foo")

      expect(result.warnings.to_h).to eql(email: ["uses a deprecated format"])
      expect(result.warning?(:email)).to be(true)
      expect(result.error?(:email)).to be(false)
    end
  end

  context "setting warnings via an explicit path" do
    context "with a nested key as a hash" do
      before do
        contract_class.rule(:email) do
          key(contact: :details).warning("is deprecated")
        end
      end

      it "sets the warning under the nested key" do
        expect(contract.(email: "foo").warnings.to_h)
          .to eql(contact: {details: ["is deprecated"]})
      end
    end
  end

  context "setting base warnings" do
    before do
      contract_class.rule(:email) do
        base.warning("input uses a legacy format")
      end
    end

    it "sets the warning under the base path" do
      result = contract.(email: "foo")

      expect(result).to be_success
      expect(result.warnings.to_h).to eql(nil => ["input uses a legacy format"])
    end
  end

  context "setting warnings with meta data" do
    before do
      contract_class.rule(:email) do
        key.warning(text: "is deprecated", code: :legacy)
      end
    end

    it "exposes meta on the warning" do
      warning = contract.(email: "foo").warnings.first

      expect(warning.text).to eql("is deprecated")
      expect(warning.meta).to eql(code: :legacy)
      expect(warning).to be_warning
    end

    it "dumps meta into the warning hash" do
      expect(contract.(email: "foo").warnings.to_h)
        .to eql(email: [text: "is deprecated", code: :legacy])
    end
  end

  context "with a localized warning id" do
    before do
      contract_class.config.messages.load_paths << SPEC_ROOT
        .join("fixtures/messages/errors.en.yml").realpath

      contract_class.rule(:email) do
        key.warning(:deprecated)
      end
    end

    it "resolves the warning translation" do
      expect(contract.(email: "foo").warnings.to_h)
        .to eql(email: ["is deprecated, please migrate"])
    end
  end

  context "with a missing translation key" do
    before do
      contract_class.rule(:email) do
        key.warning(:totally_missing)
      end
    end

    it "raises MissingMessageError just like failures do" do
      expect { contract.(email: "foo").warnings.to_h }
        .to raise_error(Dry::Validation::MissingMessageError, /totally_missing/)
    end
  end

  context "warnings on array members with nested hashes" do
    before do
      contract_class.rule(:items).each do
        key.warning(:legacy) if value[:kind] == "legacy"
      end

      contract_class.config.messages.load_paths << SPEC_ROOT
        .join("fixtures/messages/errors.en.yml").realpath
    end

    let(:input) do
      {email: "jane@doe.org", items: [{kind: "new"}, {kind: "legacy"}]}
    end

    it "sets the warning under the array index path" do
      result = contract.(input)

      expect(result).to be_success
      expect(result.errors).to be_empty
      expect(result.warnings.to_h)
        .to eql(items: {1 => ["uses a deprecated format"]})
      expect(result.warnings.first.path).to eql([:items, 1])
    end
  end

  context "the same path produces both a failure and a warning" do
    before do
      contract_class.rule(:email) do
        key.warning("legacy format, please migrate")
        key.failure("is invalid")
      end
    end

    it "fails the result while keeping both diagnostics separated" do
      result = contract.(email: "foo")

      expect(result).to be_failure
      expect(result.errors.to_h).to eql(email: ["is invalid"])
      expect(result.warnings.to_h).to eql(email: ["legacy format, please migrate"])
    end

    it "orders the failure before the warning in diagnostics" do
      result = contract.(email: "foo")

      expect(result.diagnostics[:email]).to eql(
        ["is invalid", "legacy format, please migrate"]
      )

      severity = result.diagnostics.to_a.map(&:warning?)
      expect(severity).to eql([false, true])
    end
  end

  context "a failure and a warning on different paths" do
    before do
      contract_class.rule(:email) do
        key.failure("is invalid")
      end

      contract_class.rule(:name) do
        key.warning("name will be required soon") if key?(:name)
      end
    end

    it "keeps production order in diagnostics" do
      result = contract.(email: "foo", name: "Jane")

      expect(result.diagnostics.map { |msg| [msg.path, msg.warning?] }).to eql(
        [[[:email], false], [[:name], true]]
      )
    end
  end

  context "rule dependencies" do
    it "still runs dependent rules when an earlier rule only warns" do
      contract_class.rule(:email) do
        key.warning("is deprecated")
      end

      contract_class.rule(:email) do
        key.failure("is invalid")
      end

      result = contract.(email: "foo")

      expect(result).to be_failure
      expect(result.warnings.to_h).to eql(email: ["is deprecated"])
      expect(result.errors.to_h).to eql(email: ["is invalid"])
    end

    it "runs nested rules when only a parent path warning exists" do
      nested_contract = Class.new(Dry::Validation::Contract) do
        schema do
          required(:address).hash do
            required(:city).filled(:string)
          end
        end

        rule(:address) do
          key.warning("address will be renamed to location")
        end

        rule(address: :city) do
          key.warning("city format is legacy")
        end
      end

      result = nested_contract.new.(address: {city: "Oldtown"})

      expect(result).to be_success
      expect(result.warnings.to_h).to eql(
        address: [["address will be renamed to location"], {city: ["city format is legacy"]}]
      )
    end

    it "still skips rules whose keys failed the schema, warnings do not change that" do
      contract_class.rule(:name) do
        key.warning("name is legacy")
      end

      result = contract.(email: "foo", name: 123)

      expect(result).to be_failure
      expect(result.errors.to_h).to eql(name: ["must be a string"])
      expect(result.warnings).to be_empty
    end
  end
end
