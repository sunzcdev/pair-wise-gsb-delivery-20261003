# frozen_string_literal: true

RSpec.describe Dry::Transaction::StepAdapters::Scope, :adapter do
  subject { described_class.new }

  let(:options) { {step_name: "unit"} }
  let(:released) { [] }
  let(:release) { -> { released << :released } }
  let(:resource) { Dry::Transaction::ScopedResource["handle", release] }
  let(:operation) { -> input { resource } }

  let(:continue) do
    -> (input) { input.fmap { |v| "#{v} terminated" } }
  end

  def call(&block)
    subject.(operation, options, ["input"], &(block || continue))
  end

  describe "#call" do
    it "hands the handle to the block and returns the block's result" do
      expect(call).to eql(Success("handle terminated"))
    end

    it "releases the resource" do
      call
      expect(released).to eql([:released])
    end

    context "when the block returns a Failure" do
      let(:continue) { -> input { Failure("nope") } }

      it "returns the Failure and still releases the resource" do
        expect(call).to eql(Failure("nope"))
        expect(released).to eql([:released])
      end
    end

    context "when the block raises" do
      let(:error) { Class.new(StandardError) }
      let(:continue) { -> input { raise error } }

      it "propagates the exception and still releases the resource" do
        expect { call }.to raise_error(error)
        expect(released).to eql([:released])
      end
    end

    context "when the operation returns a bare handle" do
      let(:handle) do
        released = self.released
        Object.new.tap { |object|
          object.define_singleton_method(:close) { released << :closed }
        }
      end
      let(:operation) { -> input { handle } }

      it "releases it via #close" do
        call
        expect(released).to eql([:closed])
      end
    end

    context "when the operation returns a Result" do
      context "with Success(resource)" do
        let(:operation) { -> input { Success(resource) } }

        it "scopes the resource" do
          expect(call).to eql(Success("handle terminated"))
          expect(released).to eql([:released])
        end
      end

      context "with Failure" do
        let(:operation) { -> input { Failure(:unavailable) } }

        it "returns the Failure without running the block or releasing" do
          expect(call).to eql(Failure(:unavailable))
          expect(released).to be_empty
        end
      end
    end

    context "when the release fails" do
      let(:release_error) { StandardError.new("cannot release") }
      let(:release) { -> { raise release_error } }

      it "raises a CleanupError preserving the result and the release failure" do
        expect { call }.to raise_error(Dry::Transaction::CleanupError) { |error|
          expect(error.result).to eql(Success("handle terminated"))
          expect(error.errors).to eql([release_error])
          expect(error.cause).to be_nil
        }
      end

      context "and the block raised as well" do
        let(:flow_error) { RuntimeError.new("flow failed") }
        let(:continue) { -> input { raise flow_error } }

        it "raises a CleanupError preserving the flow exception as its cause" do
          expect { call }.to raise_error(Dry::Transaction::CleanupError) { |error|
            expect(error.result).to be_nil
            expect(error.errors).to eql([release_error])
            expect(error.cause).to be(flow_error)
          }
        end
      end
    end

    context "when an inner scope reported release failures" do
      let(:inner_error) { StandardError.new("inner release failed") }
      let(:continue) do
        -> input {
          raise Dry::Transaction::CleanupError.new([inner_error], result: input)
        }
      end

      it "merges them with its own release failure" do
        own_error = StandardError.new("own release failed")
        release = -> { raise own_error }
        operation = -> input { Dry::Transaction::ScopedResource["handle", release] }

        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(Dry::Transaction::CleanupError) { |error|
          expect(error.result).to eql(Success("handle"))
          expect(error.errors).to eql([inner_error, own_error])
        }
      end

      it "keeps the inner flow exception as the cause" do
        flow_error = RuntimeError.new("inner flow failed")
        inner_cleanup = Dry::Transaction::CleanupError.new([inner_error])
        continue = -> input { raise inner_cleanup, cause: flow_error }

        expect {
          subject.(operation, options, ["input"], &continue)
        }.to raise_error(Dry::Transaction::CleanupError) { |error|
          expect(error.errors).to eql([inner_error])
          expect(error.cause).to be(flow_error)
        }
      end
    end
  end
end
