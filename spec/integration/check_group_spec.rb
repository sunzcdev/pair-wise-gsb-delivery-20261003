# frozen_string_literal: true

RSpec.describe "check_group step" do
  let(:container) {
    Class.new do
      extend Dry::Core::Container::Mixin

      register :check_name, -> input {
        input[:name].to_s.empty? ? Dry::Monads.Failure(:name_missing) : Dry::Monads.Success(name: input[:name])
      }
      register :check_age, -> input {
        input[:age].to_i < 18 ? Dry::Monads.Failure(:age_invalid) : Dry::Monads.Success(age: input[:age])
      }
      register :check_email, -> input {
        input[:email].to_s.include?("@") ? Dry::Monads.Success(email: input[:email]) : Dry::Monads.Failure(:email_invalid)
      }
      register :persist, -> input { Test::DB << input and true }
    end
  }

  let(:transaction) {
    Class.new do
      include Dry::Transaction(container: Test::Container)

      check_group :validate, with: [:check_name, :check_age, :check_email]
      tee :persist, with: :persist
    end.new
  }

  before do
    Test::DB = Array.new
    Test::Container = container
  end

  context "when all checks pass" do
    it "merges the declared outputs and continues the transaction" do
      result = transaction.call(name: "Jane", age: 30, email: "jane@example.com")

      expect(result).to eql(Dry::Monads.Success(name: "Jane", age: 30, email: "jane@example.com"))
      expect(Test::DB).to eq [{name: "Jane", age: 30, email: "jane@example.com"}]
    end
  end

  context "when two of three checks fail" do
    let(:input) { {name: "", age: 12, email: "jane@example.com"} }

    it "runs every check and collects all failures" do
      result = transaction.call(input)

      expect(result).to be_failure
      expect(result.failure).to eq [:name_missing, :age_invalid]
    end

    it "never runs the side-effect step after the group" do
      transaction.call(input)

      expect(Test::DB).to be_empty
    end
  end

  context "when two checks declare the same output key" do
    let(:container) {
      Class.new do
        extend Dry::Core::Container::Mixin

        register :check_one, -> _input { Dry::Monads.Success(token: "one") }
        register :check_two, -> _input { Dry::Monads.Success(token: "two") }
      end
    }

    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        check_group :validate, with: [:check_one, :check_two]
      end.new
    }

    it "raises instead of silently overwriting" do
      expect { transaction.call({}) }.to raise_error(
        Dry::Transaction::DuplicateOutputKeyError, /token/
      )
    end
  end

  describe "step events" do
    let(:subscriber) do
      Class.new do
        attr_reader :started, :success, :failed

        def initialize
          @started = []
          @success = []
          @failed = []
        end

        def on_step(event)
          started << event[:step_name]
        end

        def on_step_succeeded(event)
          success << {step_name: event[:step_name], value: event[:value]}
        end

        def on_step_failed(event)
          failed << {step_name: event[:step_name], value: event[:value]}
        end
      end.new
    end

    context "subscribing to all step events" do
      before do
        transaction.subscribe(subscriber)
      end

      it "distinguishes individual check results from the group result" do
        transaction.call(name: "", age: 12, email: "jane@example.com")

        expect(subscriber.success).to eq [
          {step_name: :check_email, value: {email: "jane@example.com"}}
        ]
        expect(subscriber.failed).to eq [
          {step_name: :check_name, value: :name_missing},
          {step_name: :check_age, value: :age_invalid},
          {step_name: :validate, value: [:name_missing, :age_invalid]}
        ]
        expect(subscriber.started).not_to include(:persist)
      end

      it "publishes a group success event with the merged outputs" do
        transaction.call(name: "Jane", age: 30, email: "jane@example.com")

        expect(subscriber.success).to include(
          {step_name: :validate, value: {name: "Jane", age: 30, email: "jane@example.com"}}
        )
      end
    end

    context "subscribing to a single check within the group" do
      before do
        transaction.subscribe(check_email: subscriber)
      end

      it "receives events for that check only" do
        transaction.call(name: "", age: 12, email: "jane@example.com")

        expect(subscriber.success).to eq [
          {step_name: :check_email, value: {email: "jane@example.com"}}
        ]
        expect(subscriber.failed).to be_empty
      end
    end
  end
end
