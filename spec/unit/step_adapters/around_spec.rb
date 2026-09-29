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

    context "when the result of the operation is a Failure value" do
      let(:operation) {
        -> (input, &block) { block.(Failure(input.upcase)) }
      }

      it "return a Failure value" do
        expect(subject.(operation, options, ["input"], &continue)).to eql(Failure("INPUT"))
      end
    end

    context "when the operation short-circuits without calling the continuation" do
      let(:operation) {
        -> (input, &_block) { Success("short #{input}") }
      }

      it "returns the operation result" do
        expect(subject.(operation, options, ["circuit"], &continue)).to eql(Success("short circuit"))
      end
    end
  end

  describe "strict continuation" do
    let(:continue) do
      -> (input) { input.fmap { |v| "#{v} terminated" } }
    end

    context "calling the continuation twice within one operation" do
      let(:operation) do
        -> (input, &block) {
          block.(Success(input))
          block.(Success(input))
        }
      end

      it "rejects the second call before running downstream steps again" do
        downstream_runs = 0
        continue = -> (input) {
          downstream_runs += 1
          input.fmap { |v| "#{v} terminated" }
        }

        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          "continuation for step +`unit`+ has already been called; it can only be used once"
        )
        expect(downstream_runs).to be 1
      end
    end

    context "calling the continuation after the operation has returned" do
      let(:captured) { [] }

      let(:operation) do
        -> (input, &block) {
          captured << block
          Success(input)
        }
      end

      it "rejects the stale continuation before running downstream steps" do
        downstream_runs = 0
        continue = -> (input) {
          downstream_runs += 1
          input
        }

        result = subject.(operation, options, ["input"], &continue)

        expect(result).to eql(Success("input"))
        expect {
          captured.first.(Success("later"))
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          "continuation for step +`unit`+ can no longer be called after the around step has returned"
        )
        expect(downstream_runs).to be 0
      end
    end

    context "reusing one invocation's continuation in a later transaction call" do
      let(:captured) { [] }

      let(:first_operation) do
        -> (input, &block) {
          captured << block
          block.(Success(input))
        }
      end

      let(:second_operation) do
        -> (input, &block) {
          captured.first.(Success(input))
          block.(Success(input))
        }
      end

      it "rejects the foreign continuation using its own step name" do
        second_options = {step_name: "other"}
        downstream_runs = 0
        continue = -> (input) {
          downstream_runs += 1
          input
        }

        subject.(first_operation, options, ["one"], &continue)

        expect {
          subject.(second_operation, second_options, ["two"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidContinuationError,
          "continuation for step +`unit`+ can no longer be called after the around step has returned"
        )
        expect(downstream_runs).to be 1
      end
    end

    context "with two concurrent calls in flight" do
      it "keeps each continuation isolated and runs downstream steps once per call" do
        downstream_runs = Hash.new(0)
        continue = -> (label) {
          -> (input) {
            downstream_runs[label] += 1
            input
          }
        }

        operations = {}
        ready = Queue.new
        go = Queue.new
        results = {}
        errors = {}
        repeat_errors = {}

        operations[:a] = -> (input, &block) {
          ready << :a
          go.pop
          block.(Success(input))
          begin
            block.(Success(input))
          rescue Dry::Transaction::InvalidContinuationError => error
            repeat_errors[:a] = error
          end
          Success(input)
        }

        operations[:b] = -> (input, &block) {
          ready << :b
          go.pop
          block.(Success(input))
          Success(input)
        }

        threads = [:a, :b].map { |label|
          Thread.new {
            begin
              results[label] = subject.(
                operations[label],
                {step_name: label.to_s},
                ["input-#{label}"],
                &continue.(label)
              )
            rescue => error
              errors[label] = error
            end
          }
        }

        2.times { ready.pop }
        2.times { go << :start }
        threads.each(&:join)

        expect(errors).to be_empty
        expect(repeat_errors[:a].message).to include("step +`a`+")
        expect(results[:a]).to eql(Success("input-a"))
        expect(results[:b]).to eql(Success("input-b"))
        expect(downstream_runs[:a]).to be 1
        expect(downstream_runs[:b]).to be 1
      end
    end
  end
end
