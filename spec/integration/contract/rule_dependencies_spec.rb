# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::Contract, "rule dependencies" do
  subject(:contract) { contract_class.new }

  let(:calls) { [] }

  let(:contract_class) do
    recorded_calls = calls

    Class.new(Dry::Validation::Contract) do
      define_method(:record) { |name| recorded_calls << name }

      params do
        required(:account_id).filled(:integer)
        required(:quota).filled(:integer)
        required(:email).filled(:string)
        required(:login).filled(:string)
      end
    end
  end

  describe "scheduling" do
    before do
      contract_class.rule(:account_id, name: :account_available) do
        record(:account_available)
        key.failure("account is not available") if value > 1000
      end

      contract_class.rule(:email, name: :email_format) do
        record(:email_format)
        key.failure("has invalid format") unless value.include?("@")
      end

      contract_class.rule(:quota, name: :within_quota, depends_on: :account_available) do
        record(:within_quota)
        key.failure("quota exceeded") if value > 100
      end

      contract_class.rule(:login, name: :login_unique, depends_on: :email_format) do
        record(:login_unique)
        key.failure("is taken") if value == "taken"
      end
    end

    let(:input) do
      {account_id: 1, quota: 50, email: "jane@doe.org", login: "jane"}
    end

    it "runs two independent chains in stable definition order" do
      expect(contract.(input)).to be_success
      expect(calls).to eql(%i[account_available email_format within_quota login_unique])
    end

    context "when a prerequisite fails" do
      let(:input) do
        {account_id: 2000, quota: 50, email: "jane@doe.org", login: "jane"}
      end

      it "skips its dependents and records the reason" do
        result = contract.(input)

        expect(calls).to eql(%i[account_available email_format login_unique])
        expect(result.skipped_rules).to eql(
          within_quota: "dependency :account_available failed"
        )
      end

      it "does not turn skipped rules into field errors" do
        result = contract.(input)

        expect(result.errors.to_h).to eql(account_id: ["account is not available"])
        expect(result).to be_failure
      end

      it "keeps running the independent branch" do
        result = contract.(input.merge(login: "taken"))

        expect(result.errors.to_h).to eql(
          account_id: ["account is not available"],
          login: ["is taken"]
        )
      end
    end

    context "when a dependency chain is skipped transitively" do
      before do
        contract_class.rule(:quota, name: :quota_audit, depends_on: :within_quota) do
          record(:quota_audit)
          key.failure("audit failed")
        end
      end

      let(:input) do
        {account_id: 2000, quota: 50, email: "jane@doe.org", login: "jane"}
      end

      it "skips the whole chain and records each reason" do
        result = contract.(input)

        expect(calls).to eql(%i[account_available email_format login_unique])
        expect(result.skipped_rules).to eql(
          within_quota: "dependency :account_available failed",
          quota_audit: "dependency :within_quota was skipped"
        )
        expect(result.errors.to_h).to eql(account_id: ["account is not available"])
      end
    end
  end

  describe "schema error gate" do
    before do
      contract_class.rule(:account_id, name: :account_available) do
        record(:account_available)
        key.failure("account is not available")
      end

      contract_class.rule(:quota, name: :within_quota, depends_on: :account_available) do
        record(:within_quota)
        key.failure("quota exceeded") if value > 100
      end
    end

    it "does not run rules whose keys have schema errors" do
      result = contract.(account_id: "oops", quota: 50, email: "jane@doe.org", login: "jane")

      expect(calls).to eql(%i[within_quota])
      expect(result.skipped_rules).to be_empty
      expect(result.errors.to_h).to eql(account_id: ["must be an integer"])
    end
  end

  describe "definition-time checks" do
    it "raises on an unknown dependency" do
      expect {
        contract_class.rule(:quota, name: :within_quota, depends_on: :missing_rule) do
          key.failure("quota exceeded")
        end
      }.to raise_error(
        Dry::Validation::UnknownRuleDependencyError,
        /depends on an unknown rule :missing_rule/
      )
    end

    it "raises on a self-dependency cycle" do
      expect {
        contract_class.rule(:quota, name: :within_quota, depends_on: :within_quota) do
          key.failure("quota exceeded")
        end
      }.to raise_error(
        Dry::Validation::CyclicRuleDependencyError,
        /dependency cycle detected/
      )
    end

    it "raises on a mutual dependency cycle" do
      contract_class.rule(:quota, name: :first) { key.failure("nope") }
      contract_class.rule(:quota, name: :second, depends_on: :first) { key.failure("nope") }

      expect {
        contract_class.rule(:quota, name: :first, depends_on: :second) { key.failure("nope") }
      }.to raise_error(
        Dry::Validation::CyclicRuleDependencyError,
        /dependency cycle detected/
      )
    end
  end
end
