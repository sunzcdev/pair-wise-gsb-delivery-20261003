require "spec_helper"

RSpec.describe "cancellation" do
  let(:logs) { [] }
  let(:signal) { Interactor::Cancellation.new }
  let(:context) { Interactor::Context.new(logs: logs) }

  def build_step(name, cancel: false, raise_error: false, rollback_error: false)
    Class.new do
      include Interactor

      define_method(:call) do
        context.logs << :"call_#{name}"
        context.cancellation.cancel("cancel after #{name}") if cancel
        raise "error after #{name}" if raise_error
      end

      define_method(:rollback) do
        context.logs << :"rollback_#{name}"
        raise "rollback #{name}" if rollback_error
      end

      define_method(:around_step) do |interactor|
        context.logs << :"around_before_#{name}"
        interactor.call
        context.logs << :"around_after_#{name}"
      end

      around :around_step
    end
  end

  def build_organizer(*interactors)
    Class.new do
      include Interactor::Organizer
      organize(*interactors)
    end
  end

  describe "pre-cancellation" do
    let(:first_step) { build_step(:first) }
    let(:second_step) { build_step(:second) }
    let(:organizer) { build_organizer(first_step, second_step) }

    it "checks before the first step" do
      signal.cancel("request gone")

      result = organizer.call(context, signal)

      expect(logs).to eq([])
      expect(result).not_to be_failure
      expect(result).not_to be_success
      expect(result).to be_cancelled
      expect(result.cancellation_reason).to eq("request gone")
    end
  end

  describe "cancellation after a completed step" do
    let(:first_step) { build_step(:first) }
    let(:second_step) do
      build_step(:second, cancel: true)
    end
    let(:third_step) { build_step(:third) }
    let(:organizer) { build_organizer(first_step, second_step, third_step) }

    it "records the completed step, stops, and rolls back in reverse" do
      result = organizer.call(context, cancellation: signal)

      expect(logs).to eq([
        :around_before_first, :call_first, :around_after_first,
        :around_before_second, :call_second, :around_after_second,
        :rollback_second,
        :rollback_first
      ])
      expect(result).to be_cancelled
      expect(result).not_to be_failure
      expect(result.cancellation_reason).to eq("cancel after second")
    end

    it "does not run later steps" do
      organizer.call(context, signal)

      expect(logs).not_to include(:call_third)
    end
  end

  describe "rollback errors" do
    let(:first_step) { build_step(:first) }
    let(:second_step) do
      build_step(:second, cancel: true, rollback_error: true)
    end
    let(:organizer) { build_organizer(first_step, second_step) }

    it "raises the rollback error and retains cancellation" do
      expect {
        organizer.call(context, signal)
      }.to raise_error("rollback second")

      expect(logs).to eq([
        :around_before_first, :call_first, :around_after_first,
        :around_before_second, :call_second, :around_after_second,
        :rollback_second
      ])
      expect(context).to be_cancelled
      expect(context).not_to be_failure
      expect(context.cancellation_reason).to eq("cancel after second")
    end
  end

  describe "a normal exception and cancellation" do
    let(:first_step) { build_step(:first) }
    let(:second_step) do
      build_step(:second, cancel: true, raise_error: true)
    end
    let(:organizer) { build_organizer(first_step, second_step) }

    it "keeps the ordinary exception and cancellation reason" do
      expect {
        organizer.call(context, signal)
      }.to raise_error("error after second")

      expect(logs).to eq([
        :around_before_first, :call_first, :around_after_first,
        :around_before_second, :call_second,
        :rollback_first
      ])
      expect(context).to be_cancelled
      expect(context.cancellation_reason).to eq("cancel after second")
    end
  end

  describe "a later call with a new signal" do
    let(:first_step) { build_step(:first) }
    let(:second_step) { build_step(:second) }
    let(:organizer) { build_organizer(first_step, second_step) }

    it "is isolated from a previously cancelled call" do
      signal.cancel("old request")
      organizer.call(context, signal)

      new_context = Interactor::Context.new(logs: [])
      new_signal = Interactor::Cancellation.new
      result = organizer.call(new_context, new_signal)

      expect(result).to be_success
      expect(result).not_to be_cancelled
      expect(result.cancellation_reason).to be_nil
      expect(signal).to be_cancelled
      expect(new_context.logs).to eq([
        :around_before_first, :call_first, :around_after_first,
        :around_before_second, :call_second, :around_after_second
      ])
    end
  end

  describe "nested organizers" do
    let(:first_step) { build_step(:first) }
    let(:second_step) do
      build_step(:second, cancel: true)
    end
    let(:third_step) { build_step(:third) }
    let(:inner_organizer) { build_organizer(first_step, second_step) }
    let(:organizer) do
      Class.new do
        include Interactor::Organizer
      end.tap do |built_organizer|
        built_organizer.organize(inner_organizer, third_step)
        built_organizer.around do |interactor|
          context.logs << :around_before_outer
          interactor.call
          context.logs << :around_after_outer
        end
      end
    end

    it "shares the signal and unwinds around exits before rollback" do
      result = organizer.call(context, signal)

      expect(logs).to eq([
        :around_before_outer,
        :around_before_first, :call_first, :around_after_first,
        :around_before_second, :call_second, :around_after_second,
        :around_after_outer,
        :rollback_second,
        :rollback_first
      ])
      expect(result).to be_cancelled
      expect(result.cancellation).to equal(signal)
    end
  end
end
