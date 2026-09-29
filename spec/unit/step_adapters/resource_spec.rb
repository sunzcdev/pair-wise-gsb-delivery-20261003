# frozen_string_literal: true

RSpec.describe Dry::Transaction::StepAdapters::Resource, :adapter do
  subject { described_class.new }

  let(:options) { {step_name: "unit"} }
  let(:log) { [] }
  let(:handle) { FakeResource.new(:unit, log) }
  let(:operation) { ->(_input) { handle } }
  let(:continue) { ->(input) { input.fmap { |h| h.use.name } } }

  describe "#call" do
    it "hands the acquired handle to the remaining steps" do
      expect(subject.(operation, options, ["input"], &continue)).to eql(Success(:unit))
    end

    it "releases the handle after the remaining steps succeed" do
      subject.(operation, options, ["input"], &continue)

      expect(log).to eql([[:acquire, :unit], [:use, :unit, :work], [:release, :unit]])
    end

    context "when the remaining steps fail" do
      let(:continue) { ->(_input) { Failure(:nope) } }

      it "releases the handle and returns the failure" do
        expect(subject.(operation, options, ["input"], &continue)).to eql(Failure(:nope))
        expect(log.last).to eql([:release, :unit])
      end
    end

    context "when the remaining steps raise" do
      let(:continue) { ->(_input) { raise "boom" } }

      it "releases the handle and re-raises" do
        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error("boom")

        expect(log.last).to eql([:release, :unit])
      end
    end

    context "when the result of the remaining steps is NOT a Dry::Monads::Result" do
      let(:continue) { ->(_input) { "plain string" } }

      it "releases the handle and raises an InvalidResultError" do
        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(
          Dry::Transaction::InvalidResultError,
          "step +unit+ must return a Result object"
        )

        expect(log.last).to eql([:release, :unit])
      end
    end

    context "when releasing the handle fails" do
      let(:handle) {
        FakeResource.new(:unit, log, release_error: StandardError.new("release failed"))
      }

      it "raises a ResourceReleaseError preserving the main result and the release error" do
        error = nil

        begin
          subject.(operation, options, ["input"], &continue)
        rescue Dry::Transaction::ResourceReleaseError => e
          error = e
        end

        expect(error.result).to eql(Success(:unit))
        expect(error.release_errors.map(&:message)).to eql(["release failed"])
      end
    end
  end
end
