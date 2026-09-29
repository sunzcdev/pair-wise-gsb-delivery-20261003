# frozen_string_literal: true

RSpec.describe "around steps with a strict single-use continuation" do
  include Dry::Monads[:result]

  let(:container) do
    Class.new do
      extend Dry::Core::Container::Mixin
      extend Dry::Monads[:result]
    end
  end

  let(:transaction_class) do
    container = self.container

    Class.new do
      include Dry::Transaction(container: container)

      around :outer, with: "outer"
      around :inner, with: "inner"
      step :work, with: "work"
    end
  end

  attr_reader :work_calls

  let(:work) do
    -> input {
      @work_calls += 1
      Success(input.merge(work_calls: @work_calls))
    }
  end

  let(:normal_outer) do
    -> (input, &block) { block.(Success(input)) }
  end

  let(:normal_inner) do
    -> (input, &block) { block.(Success(input)) }
  end

  before do
    @work_calls = 0
    container.register(:work, work)
    container.register(:outer, normal_outer)
    container.register(:inner, normal_inner)
  end

  describe "nested around steps" do
    it "runs each continuation and the following steps exactly once" do
      result = transaction_class.new.call({ok: true})

      expect(result).to eql(Success({ok: true, work_calls: 1}))
      expect(work_calls).to be 1
    end

    it "rejects a repeated call on the inner continuation before re-running work" do
      captured = nil

      inner = -> (input, &block) {
        captured = block
        block.(Success(input))
      }

      transaction = transaction_class.new(inner: inner)

      expect {
        transaction.call({})
        captured.(Success({}))
      }.to raise_error(
        Dry::Transaction::InvalidContinuationError,
        "continuation for step +`inner`+ can no longer be called after the around step has returned"
      )

      expect(work_calls).to be 1
    end

    it "rejects a repeated call on the outer continuation and names the outer step" do
      inner = -> (input, &block) { block.(Success(input)) }

      outer = -> (input, &block) {
        block.(Success(input))
        block.(Success(input))
      }

      expect {
        transaction_class.new(outer: outer, inner: inner).call({})
      }.to raise_error(
        Dry::Transaction::InvalidContinuationError,
        "continuation for step +`outer`+ has already been called; it can only be used once"
      )

      expect(work_calls).to be 1
    end

    it "allows a short-circuit that never calls the continuation" do
      inner = -> (input, &_block) { Failure(input.merge(short: true)) }

      result = transaction_class.new(inner: inner).call({})

      expect(result).to eql Failure({short: true})
      expect(work_calls).to be 0
    end
  end

  describe "exiting via an exception" do
    it "invalidates the continuations and does not re-run the following steps" do
      boom = Class.new(StandardError)
      captured = {}

      outer = -> (input, &block) {
        captured[:outer] = block
        block.(Success(input))
      }

      inner = -> (input, &block) {
        captured[:inner] = block
        block.(Success(input))
      }

      failing_work = -> _input { raise boom, "boom" }

      expect {
        transaction_class.new(outer: outer, inner: inner, work: failing_work).call({})
      }.to raise_error(boom)

      expect(work_calls).to be 0

      expect {
        captured[:inner].(Success({}))
      }.to raise_error(Dry::Transaction::InvalidContinuationError, /step \+`inner`+/)

      expect {
        captured[:outer].(Success({}))
      }.to raise_error(Dry::Transaction::InvalidContinuationError, /step \+`outer`+/)

      expect(work_calls).to be 0
    end
  end

  describe "two concurrent calls" do
    it "isolates the continuations and runs the following steps once in each call" do
      ready = Queue.new
      go = Queue.new
      results = {}
      errors = {}

      build_around = -> (label) {
        -> (input, &block) {
          ready << label
          go.pop
          block.(Success(input))
        }
      }

      build_work = -> (label) {
        calls = 0
        -> input {
          calls += 1
          Success(input.merge(work: label, work_calls: calls))
        }
      }

      threads = [:one, :two].map { |label|
        Thread.new {
          begin
            transaction = transaction_class.new(
              outer: -> (input, &block) { block.(Success(input)) },
              inner: build_around.(label),
              work: build_work.(label)
            )
            results[label] = transaction.call(id: label)
          rescue => error
            errors[label] = error
          end
        }
      }

      2.times { ready.pop }
      2.times { go << :start }
      threads.each(&:join)

      expect(errors).to be_empty
      expect(results[:one]).to eql(Success(id: :one, work: :one, work_calls: 1))
      expect(results[:two]).to eql(Success(id: :two, work: :two, work_calls: 1))
    end
  end
end
