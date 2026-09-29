# frozen_string_literal: true

RSpec.describe "scope steps" do
  include_context "container"

  include Dry::Monads[:result]

  before do
    Test::UncaughtError = Class.new(StandardError)
    Test::ReleaseError = Class.new(StandardError)

    # In-memory fake resource recording its own lifecycle
    Test::FakeResource = Class.new do
      attr_reader :name

      def initialize(name, log, release_error: nil)
        @name = name
        @log = log
        @release_error = release_error
      end

      def use
        @log << [:use, name]
        name
      end

      def release
        @log << [:release, name]
        raise @release_error if @release_error
      end
    end
  end

  let(:log) { [] }
  let(:resources) { {} }
  let(:received) { {} }
  let(:input) { {name: "Jane"} }

  let(:transaction) do
    Class.new do
      include Dry::Transaction(container: Test::Container)

      scope :outer
      scope :inner
      step :work
    end
  end

  def acquire(name, release_error: nil)
    -> input {
      log << [:acquire, name]
      received[name] = input
      resource = Test::FakeResource.new(name, log, release_error: release_error)
      resources[name] = resource
      Dry::Transaction::ScopedResource[resource, :release]
    }
  end

  let(:acquire_outer) { acquire(:outer) }
  let(:acquire_inner) { acquire(:inner) }

  let(:work) do
    -> handle {
      handle.use
      Success(handle)
    }
  end

  def call_transaction(**operations)
    transaction.new(
      **{outer: acquire_outer, inner: acquire_inner, work: work}.merge(operations)
    ).call(input)
  end

  it "passes each handle to the nested steps" do
    call_transaction

    expect(received[:outer]).to eql(input)
    expect(received[:inner]).to be(resources[:outer])
  end

  context "when the steps succeed" do
    it "acquires, uses and releases the resources in order" do
      result = call_transaction

      expect(result).to eql(Success(resources[:inner]))
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when a step fails" do
    let(:work) do
      -> handle {
        handle.use
        Failure(:work_failed)
      }
    end

    it "returns the Failure and releases the resources in reverse order" do
      result = call_transaction

      expect(result).to eql(Failure(:work_failed))
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when a step raises" do
    let(:work) do
      -> handle {
        handle.use
        raise Test::UncaughtError
      }
    end

    it "propagates the exception and releases the resources in reverse order" do
      expect { call_transaction }.to raise_error(Test::UncaughtError)

      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when an around step short-circuits the remaining steps" do
    let(:transaction) do
      Class.new do
        include Dry::Transaction(container: Test::Container)

        scope :outer
        scope :inner
        around :halt
        step :work
      end
    end

    let(:halt) { -> input, &block { Failure(:halted) } }

    it "releases the resources in reverse order without running the steps" do
      result = transaction.new(
        outer: acquire_outer, inner: acquire_inner, halt: halt, work: work
      ).call(input)

      expect(result).to eql(Failure(:halted))
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when the inner resource cannot be acquired" do
    context "with a Failure" do
      let(:acquire_inner) { -> input { Failure(:inner_unavailable) } }

      it "short-circuits and releases the outer resource" do
        result = call_transaction

        expect(result).to eql(Failure(:inner_unavailable))
        expect(log).to eql([
          [:acquire, :outer],
          [:release, :outer]
        ])
      end
    end

    context "with an exception" do
      let(:acquire_inner) { -> input { raise Test::UncaughtError } }

      it "propagates the exception and releases the outer resource" do
        expect { call_transaction }.to raise_error(Test::UncaughtError)

        expect(log).to eql([
          [:acquire, :outer],
          [:release, :outer]
        ])
      end
    end
  end

  context "when a release fails" do
    let(:acquire_inner) { acquire(:inner, release_error: Test::ReleaseError.new("inner boom")) }

    it "runs the remaining releases and preserves result and cleanup errors" do
      expect { call_transaction }.to raise_error(Dry::Transaction::CleanupError) { |error|
        expect(error.result).to eql(Success(resources[:inner]))
        expect(error.errors.map(&:message)).to eql(["inner boom"])
        expect(error.cause).to be_nil
      }

      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end

    context "and the outer release fails too" do
      let(:acquire_outer) { acquire(:outer, release_error: Test::ReleaseError.new("outer boom")) }

      it "collects both cleanup errors in release order" do
        expect { call_transaction }.to raise_error(Dry::Transaction::CleanupError) { |error|
          expect(error.result).to eql(Success(resources[:inner]))
          expect(error.errors.map(&:message)).to eql(["inner boom", "outer boom"])
        }

        expect(log).to eql([
          [:acquire, :outer],
          [:acquire, :inner],
          [:use, :inner],
          [:release, :inner],
          [:release, :outer]
        ])
      end
    end

    context "and a step raised as well" do
      let(:work) { -> handle { raise Test::UncaughtError } }

      it "preserves the flow exception as the cleanup error's cause" do
        expect { call_transaction }.to raise_error(Dry::Transaction::CleanupError) { |error|
          expect(error.result).to be_nil
          expect(error.errors.map(&:message)).to eql(["inner boom"])
          expect(error.cause).to be_a(Test::UncaughtError)
        }

        expect(log).to eql([
          [:acquire, :outer],
          [:acquire, :inner],
          [:release, :inner],
          [:release, :outer]
        ])
      end
    end
  end
end
