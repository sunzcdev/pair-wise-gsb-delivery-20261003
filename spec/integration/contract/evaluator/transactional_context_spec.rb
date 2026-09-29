# frozen_string_literal: true

RSpec.describe Dry::Validation::Evaluator, "transactional context" do
  subject(:contract) { contract_class.new }

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      config.transactional_context = true

      schema do
        required(:user_id).filled(:integer)
        required(:email).filled(:string)
      end

      rule(:user_id) do |context:|
        context[:user] = "jane"
        key.failure("must be jane") unless values[:user_id].equal?(312)
      end

      rule(:email) do |context:|
        key.failure("is invalid") if context[:user] == "jane" && values[:email] != "jane@doe.org"
      end
    end
  end

  it "merges context changes from successful rules" do
    result = contract.(user_id: 312, email: "john@doe.org")

    expect(result.errors.to_h).to eql(email: ["is invalid"])
    expect(result.context.each.to_h).to eql(user: "jane")
  end

  it "rolls back context changes from a failing rule" do
    result = contract.(user_id: 3, email: "john@doe.org")

    expect(result.errors.to_h).to eql(user_id: ["must be jane"])
    expect(result.context.each.to_h).to eql({})
  end

  it "exposes only the committed view to a rule following a failing rule" do
    seen = []

    contract_class.rule(:email) do |context:|
      seen << context.each.to_h
    end

    contract.(user_id: 3, email: "jane@doe.org")

    expect(seen).to eql([{}])
  end

  context "with rules writing to the initial context" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        config.transactional_context = true

        schema do
          required(:email).filled(:string)
        end
      end
    end

    it "rolls back key deletions from a failing rule" do
      seen = []

      contract_class.rule(:email) do |context:|
        context.delete(:name)
        key.failure("nope")
      end

      contract_class.rule(:email) do |context:|
        seen << context.each.to_h
      end

      result = contract.({email: "jane@doe.org"}, {name: "John"})

      expect(seen).to eql([{name: "John"}])
      expect(result.context.each.to_h).to eql(name: "John")
    end

    it "rolls back nested hash and array mutations from a failing rule" do
      seen = []

      contract_class.rule(:email) do |context:|
        context[:stats][:count] += 1
        context[:tags] << "admin"
        key.failure("nope")
      end

      contract_class.rule(:email) do |context:|
        seen << context.each.to_h
      end

      result = contract.(
        {email: "jane@doe.org"},
        {stats: {count: 0}, tags: []}
      )

      expect(seen).to eql([{stats: {count: 0}, tags: []}])
      expect(result.context.each.to_h).to eql(stats: {count: 0}, tags: [])
    end

    it "merges nested hash and array mutations from a successful rule" do
      contract_class.rule(:email) do |context:|
        context[:stats][:count] += 1
        context[:tags] << "admin"
      end

      result = contract.(
        {email: "jane@doe.org"},
        {stats: {count: 0}, tags: []}
      )

      expect(result.context.each.to_h).to eql(stats: {count: 1}, tags: ["admin"])
    end

    it "rolls back context changes when a rule raises" do
      contract_class.rule(:email) do |context:|
        context[:stats][:count] += 1
        context[:user] = "jane"
        raise "boom"
      end

      initial = {stats: {count: 0}}

      expect { contract.({email: "jane@doe.org"}, initial) }.to raise_error("boom")
      expect(initial).to eql(stats: {count: 0})
    end
  end

  context "when the same contract is called multiple times" do
    subject(:contract) do
      contract_class.new(default_context: {stats: {count: 0}})
    end

    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        config.transactional_context = true

        schema do
          required(:email).filled(:string)
        end

        rule(:email) do |context:|
          context[:stats][:count] += 1
          context[:user] = "jane"
        end
      end
    end

    it "does not leak committed changes between calls" do
      first = contract.(email: "jane@doe.org")
      second = contract.(email: "jane@doe.org")

      expect(first.context.each.to_h).to eql(stats: {count: 1}, user: "jane")
      expect(second.context.each.to_h).to eql(stats: {count: 1}, user: "jane")
    end

    it "does not mutate the default context" do
      contract.(email: "jane@doe.org")

      expect(contract.default_context).to eql(stats: {count: 0})
    end
  end

  context "when enabled via the constructor" do
    subject(:contract) { contract_class.new(transactional_context: true) }

    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        schema do
          required(:user_id).filled(:integer)
        end

        rule(:user_id) do |context:|
          context[:user] = "jane"
          key.failure("must be jane")
        end
      end
    end

    it "rolls back context changes from a failing rule" do
      expect(contract.(user_id: 3).context.each.to_h).to eql({})
    end
  end

  context "with a rule using each" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        config.transactional_context = true

        schema do
          required(:tags).array(:string)
        end

        rule(:tags).each do |context:, index:|
          context[:seen] = index
          key.failure("nope") if values[:tags][index] == "bad"
        end
      end
    end

    it "merges writes when every element passes" do
      expect(contract.(tags: ["good"]).context.each.to_h).to eql(seen: 0)
    end

    it "rolls back writes when any element fails" do
      expect(contract.(tags: %w[good bad]).context.each.to_h).to eql({})
    end
  end

  context "when not enabled" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        schema do
          required(:user_id).filled(:integer)
          required(:email).filled(:string)
        end

        rule(:user_id) do |context:|
          context[:user] = "jane"
          key.failure("must be jane")
        end

        rule(:email) do |context:|
          key.failure("is invalid") if context[:user] == "jane"
        end
      end
    end

    it "keeps context changes from a failing rule visible to other rules" do
      result = contract.(user_id: 3, email: "jane@doe.org")

      expect(result.errors.to_h).to eql(user_id: ["must be jane"], email: ["is invalid"])
      expect(result.context.each.to_h).to eql(user: "jane")
    end
  end
end
