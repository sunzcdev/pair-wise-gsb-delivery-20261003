# frozen_string_literal: true

RSpec.describe "Nested transactions" do
  include_context "database"

  # Two sub-transactions, each with its own `validate` step
  let(:validate_account) {
    Class.new {
      include Dry::Transaction

      step :validate

      def validate(input)
        input[:name] ? Success(input) : Failure(account: ["name is missing"])
      end
    }.new
  }

  let(:validate_profile) {
    Class.new {
      include Dry::Transaction

      step :validate

      def validate(input)
        input[:bio] ? Success(input) : Failure(profile: ["bio is missing"])
      end
    }.new
  }

  let(:transaction) {
    account = validate_account
    profile = validate_profile

    Class.new {
      include Dry::Transaction

      step :account
      step :profile
    }.new(account: account, profile: profile)
  }

  context "called directly" do
    it "returns a result quacking like a regular Failure" do
      result = transaction.call({})

      expect(result).to be_a Dry::Monads::Result::Failure
      expect(result).to eq Failure(account: ["name is missing"])
    end

    it "keeps the original failure value" do
      expect(transaction.call({}).failure).to eq(account: ["name is missing"])
    end

    it "keeps the address of the failure in the first sub-transaction" do
      result = transaction.call({})

      expect(result.address).to eq([:account, :validate])
      expect(result.step_failure.value).to eq(account: ["name is missing"])
    end

    it "distinguishes the failing `validate` sub-flow by its address" do
      result = transaction.call(name: "Jane")

      expect(result.address).to eq([:profile, :validate])
      expect(result.failure).to eq(profile: ["bio is missing"])
    end

    it "keeps a single-level address for a transaction called directly" do
      result = validate_account.call({})

      expect(result.address).to eq([:validate])
      expect(result.failure).to eq(account: ["name is missing"])
    end
  end

  context "matching on failures" do
    it "matches by full address" do
      transaction.call(name: "Jane") do |m|
        m.success { raise "unexpected success" }
        m.failure([:profile, :validate]) { |value| @matched = value }
        m.failure { raise "unexpected catch-all" }
      end

      expect(@matched).to eq(profile: ["bio is missing"])
    end

    it "matches by parent-level scope" do
      transaction.call(name: "Jane") do |m|
        m.success { raise "unexpected success" }
        m.failure([:account]) { raise "unexpected account failure" }
        m.failure([:profile]) { |value| @matched = value }
      end

      expect(@matched).to eq(profile: ["bio is missing"])
    end

    it "still matches by leaf step name" do
      transaction.call(name: "Jane") do |m|
        m.success { raise "unexpected success" }
        m.failure(:validate) { |value| @matched = value }
        m.failure { raise "unexpected catch-all" }
      end

      expect(@matched).to eq(profile: ["bio is missing"])
    end

    it "resolves overlapping matches in definition order" do
      transaction.call(name: "Jane") do |m|
        m.success { raise "unexpected success" }
        m.failure([:profile, :validate]) { @matched = "full address" }
        m.failure([:profile]) { @matched = "parent scope" }
        m.failure { @matched = "catch-all" }
      end

      expect(@matched).to eq("full address")
    end

    it "prefers a broader case when it is defined first" do
      transaction.call(name: "Jane") do |m|
        m.success { raise "unexpected success" }
        m.failure([:profile]) { @matched = "parent scope" }
        m.failure([:profile, :validate]) { @matched = "full address" }
        m.failure { @matched = "catch-all" }
      end

      expect(@matched).to eq("parent scope")
    end
  end

  context "wrapped multiple times" do
    let(:grandparent) {
      parent = transaction

      Class.new {
        include Dry::Transaction

        step :signup
      }.new(signup: parent)
    }

    it "keeps the full address through every level of wrapping" do
      result = grandparent.call({})

      expect(result.address).to eq([:signup, :account, :validate])
      expect(result.failure).to eq(account: ["name is missing"])
    end

    it "matches by full address and parent scopes at every level" do
      grandparent.call({}) do |m|
        m.success { raise "unexpected success" }
        m.failure([:signup, :account, :validate]) { @full = true }
        m.failure { raise "unexpected catch-all" }
      end

      grandparent.call({}) do |m|
        m.success { raise "unexpected success" }
        m.failure([:signup, :account]) { @parent = true }
        m.failure { raise "unexpected catch-all" }
      end

      grandparent.call({}) do |m|
        m.success { raise "unexpected success" }
        m.failure([:signup]) { @scope = true }
        m.failure { raise "unexpected catch-all" }
      end

      expect([@full, @parent, @scope]).to eq([true, true, true])
    end
  end

  context "failure raised as an exception" do
    let(:validate_account) {
      Class.new {
        include Dry::Transaction

        try :validate, catch: Test::NotValidError

        def validate(_input)
          raise Test::NotValidError, "account is not valid"
        end
      }.new
    }

    it "keeps the original exception along with the address" do
      result = transaction.call({})

      expect(result.failure).to be_a Test::NotValidError
      expect(result.failure.message).to eq("account is not valid")
      expect(result.step_failure.value).to be(result.failure)
      expect(result.address).to eq([:account, :validate])
    end
  end
end
