# frozen_string_literal: true

RSpec.describe Dry::Transaction::StepAdapters::Around, :adapter do
  subject { described_class.new }

  let(:operation) {
    -> (input, &block) { block.(Success(input.upcase)) }
  }

  let(:options) { {step_name: "unit"} }

  let(:continue) do
    -> (input) { input.fmap { |v| "#{v} terminated" } }
  end

  describe "#call" do
    context "when the result of the operation is NOT a Dry::Monads::Result" do
      let(:continue) do
        -> (input) { "plain string" }
      end

      it "raises an InvalidResultError" do
        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidResultError,
          "step +unit+ must return a Result object"
        )
      end
    end

    context "passing a block" do
      it "returns a Success value with result from block" do
        expect(subject.(operation, options, ["input"], &continue)).to eql(Success("INPUT terminated"))
      end
    end

    context "with strict continuations" do
      let(:options) { {step_name: "unit", strict: true} }

      let(:downstream_calls) { [] }

      let(:continue) do
        -> (input) {
          downstream_calls << input
          input.fmap { |v| "#{v} terminated" }
        }
      end

      it "allows a short-circuit without calling the continuation" do
        operation = -> (_input, &_block) { Success("short circuit") }

        result = subject.(operation, options, ["input"], &continue)

        expect(result).to eql(Success("short circuit"))
        expect(downstream_calls).to be_empty
      end

      it "rejects a repeated continuation before downstream side effects" do
        operation = -> (input, &block) {
          block.(Success(input))
          block.(Success("again"))
        }

        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          /\Astep \+unit\+ continuation can only be called once \(invocation #\d+\)\z/
        )

        expect(downstream_calls).to contain_exactly(Success("input"))
      end

      it "rejects a continuation after the operation returns" do
        saved_continuation = nil
        operation = -> (_input, &block) {
          saved_continuation = block
          Success("short circuit")
        }

        subject.(operation, options, ["input"], &continue)

        expect {
          saved_continuation.(Success("after"))
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          /\Astep \+unit\+ continuation is no longer available \(invocation #\d+\)\z/
        )
        expect(downstream_calls).to be_empty
      end

      it "does not reuse continuation state from another invocation" do
        previous_continuation = nil
        first_operation = -> (_input, &block) {
          previous_continuation = block
          Success("first")
        }

        subject.(first_operation, options, ["first"], &continue)

        second_operation = -> (input, &_block) {
          previous_continuation.(Success(input))
        }

        expect {
          subject.(second_operation, options, ["second"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          /\Astep \+unit\+ continuation is no longer available \(invocation #\d+\)\z/
        )
        expect(downstream_calls).to be_empty
      end
    end

    context "when the result of the operation is a Failure value" do
      let(:operation) {
        -> (input, &block) { block.(Failure(input.upcase)) }
      }

      it "return a Failure value" do
        expect(subject.(operation, options, ["input"], &continue)).to eql(Failure("INPUT"))
      end
    end
  end
end
