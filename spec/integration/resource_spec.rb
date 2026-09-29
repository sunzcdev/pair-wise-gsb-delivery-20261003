# frozen_string_literal: true

RSpec.describe "resource steps" do
  include_context "container"

  include Dry::Monads[:result]

  before do
    Test::WorkError = Class.new(StandardError) unless defined?(Test::WorkError)
    Test::AcquireError = Class.new(StandardError) unless defined?(Test::AcquireError)
    Test::InnerReleaseError = Class.new(StandardError) unless defined?(Test::InnerReleaseError)
    Test::OuterReleaseError = Class.new(StandardError) unless defined?(Test::OuterReleaseError)

    container.register(:outer_resource, outer_resource)
    container.register(:inner_resource, inner_resource)
  end

  let(:log) { [] }
  let(:release_errors) { {} }

  let(:outer_resource) {
    ->(_input) {
      FakeResource.new(:outer, log, release_error: release_errors[:outer])
    }
  }

  let(:inner_resource) {
    ->(_outer_handle) {
      FakeResource.new(:inner, log, release_error: release_errors[:inner])
    }
  }

  let(:transaction) do
    Class.new do
      include Dry::Transaction(container: Test::Container)

      resource :outer, with: :outer_resource
      resource :inner, with: :inner_resource
      step :work, with: :work
    end
  end

  def call_with_release_error(transaction)
    transaction.call(:input)
    nil
  rescue Dry::Transaction::ResourceReleaseError => e
    e
  end

  context "when the remaining steps succeed" do
    it "acquires, uses and releases nested resources in order" do
      work = ->(handle) { handle.use; Success(handle.name) }

      result = transaction.new(work: work).call(:input)

      expect(result).to eql(Success(:inner))
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when a step fails" do
    it "releases both resources in reverse order" do
      work = ->(handle) { handle.use; Failure(:work_failed) }

      result = transaction.new(work: work).call(:input)

      expect(result).to be_failure
      expect(result.failure).to eql(:work_failed)
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when a step raises" do
    it "releases both resources in reverse order and re-raises" do
      work = ->(handle) { handle.use; raise Test::WorkError, "work boom" }

      expect {
        transaction.new(work: work).call(:input)
      }.to raise_error(Test::WorkError, "work boom")

      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when an around step short-circuits the rest of the stack" do
    let(:transaction) do
      Class.new do
        include Dry::Transaction(container: Test::Container)

        resource :outer, with: :outer_resource
        resource :inner, with: :inner_resource
        around :guard, with: :guard
        step :work, with: :work
      end
    end

    it "releases both resources in reverse order without running later steps" do
      guard = ->(_input, &_block) { Failure(:halted) }
      work = ->(handle) { handle.use; Success(handle.name) }

      result = transaction.new(guard: guard, work: work).call(:input)

      expect(result).to be_failure
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when acquiring the inner resource fails" do
    let(:inner_resource) {
      ->(_outer_handle) { raise Test::AcquireError, "cannot acquire inner" }
    }

    it "still releases the outer resource" do
      work = ->(handle) { handle.use; Success(handle.name) }

      expect {
        transaction.new(work: work).call(:input)
      }.to raise_error(Test::AcquireError, "cannot acquire inner")

      expect(log).to eql([
        [:acquire, :outer],
        [:release, :outer]
      ])
    end
  end

  context "when the inner release fails" do
    it "still releases the outer resource and preserves result and release error" do
      release_errors[:inner] = Test::InnerReleaseError.new("inner release failed")
      work = ->(handle) { handle.use; Success(handle.name) }

      error = call_with_release_error(transaction.new(work: work))

      expect(error).to be_a(Dry::Transaction::ResourceReleaseError)
      expect(error.result).to eql(Success(:inner))
      expect(error.release_errors.map(&:message)).to eql(["inner release failed"])
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when both releases fail" do
    it "runs every release and aggregates the release errors in release order" do
      release_errors[:inner] = Test::InnerReleaseError.new("inner release failed")
      release_errors[:outer] = Test::OuterReleaseError.new("outer release failed")
      work = ->(handle) { handle.use; Success(handle.name) }

      error = call_with_release_error(transaction.new(work: work))

      expect(error).to be_a(Dry::Transaction::ResourceReleaseError)
      expect(error.result).to eql(Success(:inner))
      expect(error.release_errors.map(&:message)).to eql([
        "inner release failed",
        "outer release failed"
      ])
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end

  context "when a release fails while an exception is in flight" do
    it "preserves both the original exception and the release error" do
      release_errors[:inner] = Test::InnerReleaseError.new("inner release failed")
      work = ->(handle) { handle.use; raise Test::WorkError, "work boom" }

      error = call_with_release_error(transaction.new(work: work))

      expect(error).to be_a(Dry::Transaction::ResourceReleaseError)
      expect(error.result).to be_nil
      expect(error.release_errors.map(&:message)).to eql(["inner release failed"])
      expect(error.cause).to be_a(Test::InnerReleaseError)
      expect(error.cause.cause).to be_a(Test::WorkError)
      expect(log).to eql([
        [:acquire, :outer],
        [:acquire, :inner],
        [:use, :inner, :work],
        [:release, :inner],
        [:release, :outer]
      ])
    end
  end
end
