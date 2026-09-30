# frozen_string_literal: true

RSpec.describe Dry::Validation::Contract, "rule dependencies" do
  let(:executions) { [] }

  def build_contract(&block)
    Class.new(Dry::Validation::Contract, &block)
  end

  describe "scheduling" do
    it "runs two independent rule chains in stable dependency order" do
      executions = []
      contract_class = build_contract do
        params do
          required(:account).filled(:string)
          required(:limit).filled(:integer)
        end

        rule(:limit, name: :limit_present) { executions << :limit_present }
        rule(:account, name: :account_active) { executions << :account_active }

        rule(:account, name: :account_limit, depends_on: :account_active) do
          executions << :account_limit
        end

        rule(:limit, name: :limit_range, depends_on: :limit_present) do
          executions << :limit_range
        end
      end

      result = contract_class.new.call(account: "acc-1", limit: 100)

      expect(result).to be_success
      expect(executions).to eql(%i[limit_present account_active account_limit limit_range])
    end
  end

  describe "skipping" do
    let(:contract_class) do
      executions = self.executions

      build_contract do
        params do
          required(:account).filled(:string)
          required(:limit).filled(:integer)
        end

        rule(:account, name: :account_active) do
          executions << :account_active
          key.failure("is not active") if values[:account] == "inactive"
        end

        rule(:account, name: :account_limit, depends_on: :account_active) do
          executions << :account_limit
          key.failure("limit exceeded") if values[:account] == "over"
        end

        rule(:account, name: :account_history, depends_on: :account_limit) do
          executions << :account_history
        end

        rule(:limit, name: :limit_range) do
          executions << :limit_range
          key.failure("must be positive") if values[:limit] <= 0
        end
      end
    end

    it "skips dependents of a failed rule and records the reason" do
      result = contract_class.new.call(account: "inactive", limit: 100)

      expect(executions).to eql(%i[account_active limit_range])
      expect(result.skipped_rules).to eql(
        account_limit: "dependency :account_active failed",
        account_history: "dependency :account_limit skipped"
      )
    end

    it "does not turn skipped rules into field errors" do
      result = contract_class.new.call(account: "inactive", limit: 100)

      expect(result.errors.to_h).to eql(account: ["is not active"])
    end

    it "runs other branches normally when a dependency fails" do
      result = contract_class.new.call(account: "inactive", limit: -1)

      expect(executions).to eql(%i[account_active limit_range])
      expect(result.errors.to_h).to eql(
        account: ["is not active"],
        limit: ["must be positive"]
      )
    end

    it "runs dependents when their dependencies pass" do
      result = contract_class.new.call(account: "active", limit: 100)

      expect(executions).to eql(%i[account_active account_limit account_history limit_range])
      expect(result.skipped_rules).to be_empty
      expect(result).to be_success
    end

    it "skips only the dependents of the rule that failed" do
      result = contract_class.new.call(account: "over", limit: 100)

      expect(executions).to eql(%i[account_active account_limit limit_range])
      expect(result.skipped_rules).to eql(
        account_history: "dependency :account_limit failed"
      )
      expect(result.errors.to_h).to eql(account: ["limit exceeded"])
    end

    it "skips rules gated by schema errors as before and skips their dependents" do
      result = contract_class.new.call(account: "", limit: 100)

      expect(executions).to eql(%i[limit_range])
      expect(result.skipped_rules).to eql(
        account_limit: "dependency :account_active skipped",
        account_history: "dependency :account_limit skipped"
      )
      expect(result.errors.to_h).to eql(account: ["must be filled"])
    end
  end

  describe "definition-time validation" do
    it "raises on unknown dependencies" do
      expect {
        build_contract do
          params { required(:account).filled(:string) }

          rule(:account, name: :account_limit, depends_on: :account_active) { }
        end
      }.to raise_error(Dry::Validation::UnknownRuleDependencyError, /account_active/)
    end

    it "raises on self-dependencies" do
      expect {
        build_contract do
          params { required(:account).filled(:string) }

          rule(:account, name: :account_active, depends_on: :account_active) { }
        end
      }.to raise_error(Dry::Validation::RuleDependencyCycleError, /account_active/)
    end

    it "raises on duplicate rule names" do
      expect {
        build_contract do
          params { required(:account).filled(:string) }

          rule(:account, name: :account_active) { }
          rule(:account, name: :account_active) { }
        end
      }.to raise_error(Dry::Validation::DuplicateRuleNameError, /account_active/)
    end
  end
end
