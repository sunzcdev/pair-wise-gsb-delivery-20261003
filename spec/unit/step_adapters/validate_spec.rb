# frozen_string_literal: true

RSpec.describe Dry::Transaction::StepAdapters::Validate, :adapter do
  subject(:adapter) { described_class.new }

  let(:options) { {step_name: :validate_input, checks: checks} }

  let(:calls) { [] }

  let(:checks) {
    {
      check_name: -> input {
        calls << [:check_name, input]
        input[:name].to_s.empty? ? Failure("name is missing") : Success(name: input[:name])
      },
      check_age: -> input {
        calls << [:check_age, input]
        input[:age].to_i >= 18 ? Success(age: input[:age]) : Failure("must be an adult")
      }
    }
  }

  describe "#call" do
    it "runs every check against the same input and merges the named outputs" do
      input = {name: "Jane", age: 21}

      expect(adapter.(nil, options, [input])).to eql(Success(name: "Jane", age: 21))
      expect(calls).to eql([[:check_name, input], [:check_age, input]])
    end

    context "when checks fail" do
      let(:input) { {name: "", age: 12} }

      it "runs all checks and collects every failure" do
        expect(adapter.(nil, options, [input])).to eql(
          Failure(check_name: "name is missing", check_age: "must be an adult")
        )
        expect(calls).to eql([[:check_name, input], [:check_age, input]])
      end
    end

    context "when a check does not return a Result" do
      let(:checks) { {check_name: -> _input { "not a result" }} }

      it "raises an InvalidResultError" do
        expect { adapter.(nil, options, [{}]) }.to raise_error(
          Dry::Transaction::InvalidResultError, /check_name/
        )
      end
    end

    context "when a check's success value is not a hash of named outputs" do
      let(:checks) { {check_name: -> _input { Success("Jane") }} }

      it "raises an InvalidCheckOutputError" do
        expect { adapter.(nil, options, [{}]) }.to raise_error(
          Dry::Transaction::InvalidCheckOutputError, /check_name/
        )
      end
    end

    context "when two checks declare the same output key" do
      let(:checks) {
        {
          check_name: -> _input { Success(name: "Jane") },
          check_age: -> _input { Success(name: "Jade") }
        }
      }

      it "raises a DuplicateCheckOutputError instead of silently overwriting" do
        expect { adapter.(nil, options, [{}]) }.to raise_error(
          Dry::Transaction::DuplicateCheckOutputError, /name/
        )
      end
    end

    context "when a failing group contains duplicate output keys" do
      let(:checks) {
        {
          check_name: -> _input { Success(name: "Jane") },
          check_age: -> _input { Failure("must be an adult") },
          check_email: -> _input { Success(name: "Jade") }
        }
      }

      it "returns the failures without merging outputs" do
        expect(adapter.(nil, options, [{}])).to eql(Failure(check_age: "must be an adult"))
      end
    end

    context "without checks" do
      let(:options) { {step_name: :validate_input} }

      it "raises a MissingCheckListError" do
        expect { adapter.(nil, options, [{}]) }.to raise_error(
          Dry::Transaction::MissingCheckListError, /validate_input/
        )
      end
    end

    context "with a publisher in the options" do
      let(:publisher) {
        Class.new {
          attr_reader :events

          def initialize
            @events = []
          end

          def publish(event, payload)
            events << [event, payload]
          end
        }.new
      }

      let(:options) { {step_name: :validate_input, checks: checks, publisher: publisher} }

      it "publishes sub-check events carrying the group name" do
        input = {name: "", age: 21}
        adapter.(nil, options, [input])

        expect(publisher.events).to eql(
          [
            [:step, {step_name: :check_name, group: :validate_input, args: [input]}],
            [:step_failed, {step_name: :check_name, group: :validate_input, args: [input], value: "name is missing"}],
            [:step, {step_name: :check_age, group: :validate_input, args: [input]}],
            [:step_succeeded, {step_name: :check_age, group: :validate_input, args: [input], value: {age: 21}}]
          ]
        )
      end
    end
  end
end
