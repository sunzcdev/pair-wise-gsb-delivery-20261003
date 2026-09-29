# frozen_string_literal: true

RSpec.describe "validation groups" do
  let(:transaction) {
    Class.new do
      include Dry::Transaction

      validate :validate_input, checks: %i[check_name check_age check_email]
      tee :persist

      def check_name(input)
        input[:name].to_s.empty? ? Failure("name is missing") : Success(name: input[:name])
      end

      def check_age(input)
        input[:age].to_i >= 18 ? Success(age: input[:age]) : Failure("must be an adult")
      end

      def check_email(input)
        input[:email].to_s.include?("@") ? Success(email: input[:email]) : Failure("email is invalid")
      end

      def persist(input)
        Test::DB << input
      end
    end.new
  }

  let(:subscriber) do
    Class.new do
      attr_reader :started, :succeeded, :failed

      def initialize
        @started = []
        @succeeded = []
        @failed = []
      end

      def on_step(event)
        started << event.to_h
      end

      def on_step_succeeded(event)
        succeeded << event.to_h
      end

      def on_step_failed(event)
        failed << event.to_h
      end
    end.new
  end

  before do
    Test::DB = Array.new
  end

  context "when two of the three checks fail" do
    let(:input) { {name: "Jane", age: 12, email: "not-an-email"} }

    it "runs every check and preserves all the failures" do
      result = transaction.(input)

      expect(result).to be_failure
      expect(result.failure).to eql(
        check_age: "must be an adult",
        check_email: "email is invalid"
      )
    end

    it "never runs the side-effecting step after the group" do
      transaction.(input)

      expect(Test::DB).to be_empty
    end

    it "publishes distinguishable events for the sub-checks and the whole group" do
      transaction.subscribe(subscriber)
      transaction.(input)

      sub_check_failed = subscriber.failed.select { |event| event[:group] }
      group_failed = subscriber.failed.reject { |event| event[:group] }

      expect(sub_check_failed).to eql(
        [
          {step_name: :check_age, group: :validate_input, args: [input], value: "must be an adult"},
          {step_name: :check_email, group: :validate_input, args: [input], value: "email is invalid"}
        ]
      )

      expect(group_failed).to eql(
        [
          {
            step_name: :validate_input,
            args: [input],
            value: {check_age: "must be an adult", check_email: "email is invalid"}
          }
        ]
      )

      expect(subscriber.started.map { |event| [event[:step_name], event[:group]] }).to eql(
        [
          [:validate_input, nil],
          [:check_name, :validate_input],
          [:check_age, :validate_input],
          [:check_email, :validate_input]
        ]
      )

      expect(subscriber.succeeded).to eql(
        [
          {step_name: :check_name, group: :validate_input, args: [input], value: {name: "Jane"}}
        ]
      )
    end
  end

  context "when every check passes" do
    let(:input) { {name: "Jane", age: 21, email: "jane@doe.com"} }

    it "merges the named outputs and passes them to the next step" do
      result = transaction.(input)

      expect(result).to be_success
      expect(result.value!).to eql(name: "Jane", age: 21, email: "jane@doe.com")
      expect(Test::DB).to eql([{name: "Jane", age: 21, email: "jane@doe.com"}])
    end

    it "publishes a group success event after the sub-check events" do
      transaction.subscribe(subscriber)
      transaction.(input)

      expect(subscriber.succeeded).to eql(
        [
          {step_name: :check_name, group: :validate_input, args: [input], value: {name: "Jane"}},
          {step_name: :check_age, group: :validate_input, args: [input], value: {age: 21}},
          {step_name: :check_email, group: :validate_input, args: [input], value: {email: "jane@doe.com"}},
          {step_name: :validate_input, args: [input], value: {name: "Jane", age: 21, email: "jane@doe.com"}},
          {step_name: :persist, args: [{name: "Jane", age: 21, email: "jane@doe.com"}], value: {name: "Jane", age: 21, email: "jane@doe.com"}}
        ]
      )
    end
  end

  context "when two checks declare the same output key" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        validate :validate_input, checks: %i[check_name check_age]

        def check_name(_input)
          Success(name: "Jane")
        end

        def check_age(_input)
          Success(name: "Jade")
        end
      end.new
    }

    it "raises an error instead of silently overwriting the output" do
      expect { transaction.({}) }.to raise_error(
        Dry::Transaction::DuplicateCheckOutputError, /check_age.*name/
      )
    end
  end

  context "with checks resolved from a container" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction(container: Test::Container)

        validate :validate_input, checks: %i[check_name check_age]
      end.new
    }

    before do
      class Test::Container
        extend Dry::Core::Container::Mixin

        register :check_name, -> input { Dry::Monads::Success(name: input[:name]) }
        register :check_age, -> input { Dry::Monads::Failure("must be an adult") }
      end
    end

    it "resolves each check from the container" do
      result = transaction.(name: "Jane", age: 12)

      expect(result).to be_failure
      expect(result.failure).to eql(check_age: "must be an adult")
    end
  end

  context "with a missing check" do
    let(:transaction) {
      Class.new do
        include Dry::Transaction

        validate :validate_input, checks: %i[check_name]
      end.new
    }

    it "raises a MissingStepError" do
      expect { transaction.({}) }.to raise_error(
        Dry::Transaction::MissingStepError, /check_name/
      )
    end
  end
end
