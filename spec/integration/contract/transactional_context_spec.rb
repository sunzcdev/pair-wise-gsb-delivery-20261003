# frozen_string_literal: true

RSpec.describe Dry::Validation::Contract, "transactional context" do
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
        if values[:user_id].equal?(312)
          key.failure("missing committed user") unless context[:user] == "jane"
        else
          key.failure("user leaked") if context.key?(:user)
        end
      end
    end
  end

  it "merges context writes from a successful rule" do
    result = contract.(user_id: 312, email: "jane@doe.org")

    expect(result).to be_success
    expect(result.context.each.to_h).to eql(user: "jane")
  end

  it "discards context writes from a failed rule" do
    result = contract.(user_id: 3, email: "john@doe.org")

    expect(result.errors.to_h).to eql(user_id: ["must be jane"])
    expect(result.context.each.to_h).to eql({})
  end

  it "lets an independent rule after a failed rule read only the committed view" do
    result = contract.(user_id: 3, email: "john@doe.org")

    expect(result.errors.to_h).not_to have_key(:email)
  end

  context "with nested containers in the context" do
    subject(:contract) do
      contract_class.new(default_context: {stats: {count: 0}, tags: [], token: "secret"})
    end

    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        config.transactional_context = true

        schema do
          required(:user_id).filled(:integer)
          required(:email).filled(:string)
        end

        rule(:user_id) do |context:|
          context[:stats][:count] += 1
          context[:tags] << :from_user_id
          context.delete(:token)
          key.failure("must be jane") unless values[:user_id].equal?(312)
        end

        rule(:email) do |context:|
          if values[:user_id].equal?(312)
            key.failure("stats not merged") unless context[:stats][:count] == 1
            key.failure("tags not merged") unless context[:tags] == [:from_user_id]
            key.failure("token not deleted") if context.key?(:token)
          else
            key.failure("stats leaked") if context[:stats][:count] != 0
            key.failure("tags leaked") if context[:tags].include?(:from_user_id)
            key.failure("token deleted") unless context.key?(:token)
          end
        end
      end
    end

    it "merges nested mutations and deletions from a successful rule" do
      result = contract.(user_id: 312, email: "jane@doe.org")

      expect(result).to be_success
      expect(result.context[:stats]).to eql(count: 1)
      expect(result.context[:tags]).to eql([:from_user_id])
      expect(result.context.key?(:token)).to be(false)
    end

    it "discards nested mutations and deletions from a failed rule" do
      result = contract.(user_id: 3, email: "john@doe.org")

      expect(result.errors.to_h).to eql(user_id: ["must be jane"])
      expect(result.context[:stats]).to eql(count: 0)
      expect(result.context[:tags]).to eql([])
      expect(result.context[:token]).to eql("secret")
    end

    it "isolates two calls reusing the same contract" do
      contract.(user_id: 312, email: "jane@doe.org")
      result = contract.(user_id: 312, email: "jane@doe.org")

      expect(result.context[:stats]).to eql(count: 1)
      expect(result.context[:tags]).to eql([:from_user_id])
    end

    it "does not leak failed writes into a subsequent call reusing the contract" do
      contract.(user_id: 3, email: "john@doe.org")
      result = contract.(user_id: 3, email: "john@doe.org")

      expect(result.errors.to_h).to eql(user_id: ["must be jane"])
      expect(result.context[:stats]).to eql(count: 0)
      expect(result.context[:tags]).to eql([])
    end
  end

  context "when a rule raises an exception" do
    subject(:contract) { contract_class.new(default_context: default_context) }

    let(:default_context) { {tags: []} }

    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        config.transactional_context = true

        schema do
          required(:email).filled(:string)
        end

        rule(:email) do |context:|
          context[:tags] << :from_crashed_rule
          context[:user] = "jane"
          raise "boom"
        end
      end
    end

    it "discards the rule's writes and re-raises" do
      expect { contract.(email: "jane@doe.org") }.to raise_error(RuntimeError, "boom")
      expect(default_context[:tags]).to eql([])
    end
  end

  context "when transactional context is not enabled" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        schema do
          required(:user_id).filled(:integer)
          required(:email).filled(:string)
        end

        rule(:user_id) do |context:|
          context[:user] = "jane"
          key.failure("must be jane") unless values[:user_id].equal?(312)
        end

        rule(:email) do |context:|
          key.failure("user leaked") if context.key?(:user)
        end
      end
    end

    it "keeps writes from failed rules, preserving the default behavior" do
      result = contract.(user_id: 3, email: "john@doe.org")

      expect(result.errors.to_h).to eql(user_id: ["must be jane"], email: ["user leaked"])
      expect(result.context.each.to_h).to eql(user: "jane")
    end
  end
end
