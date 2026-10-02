module Interactor
  describe Cancellation do
    it "is not cancelled by default" do
      signal = Cancellation.new

      expect(signal.cancelled?).to eq(false)
      expect(signal.reason).to be_nil
    end

    it "is cancelled with an optional reason" do
      signal = Cancellation.new

      expect(signal.cancel!("client went away")).to eq(signal)
      expect(signal.cancelled?).to eq(true)
      expect(signal.reason).to eq("client went away")
    end

    it "is cancelled without a reason" do
      signal = Cancellation.new
      signal.cancel!

      expect(signal.cancelled?).to eq(true)
      expect(signal.reason).to be_nil
    end
  end

  describe "Organizer cancellation" do
    let(:log) { [] }
    let(:signal) { Cancellation.new }

    def build_step(name, &behavior)
      Class.new do
        include Interactor

        define_method(:call) do
          context.log << name
          instance_exec(&behavior) if behavior
        end

        define_method(:rollback) do
          context.log << [name, :rollback]
        end
      end
    end

    def build_organizer(*steps)
      Class.new do
        include Interactor::Organizer

        organize(*steps)
      end
    end

    describe "with a signal cancelled before invocation" do
      it "runs no steps and flags the context as cancelled, not failed" do
        signal.cancel!("client went away")
        organizer = build_organizer(build_step(:one), build_step(:two))

        result = organizer.call(log: log, cancellation: signal)

        expect(result).to be_cancelled
        expect(result.cancellation_reason).to eq("client went away")
        expect(result).not_to be_failure
        expect(log).to eq([])
      end

      it "raises Interactor::Cancelled with the reason from call!" do
        signal.cancel!("client went away")
        organizer = build_organizer(build_step(:one))

        expect {
          organizer.call!(log: log, cancellation: signal)
        }.to raise_error(Cancelled) { |error|
          expect(error.reason).to eq("client went away")
          expect(error.context).to be_cancelled
        }
      end
    end

    describe "when the signal is cancelled as the second step completes" do
      let(:cancelling_step) do
        build_step(:two) { context.cancellation.cancel!("stop") }
      end

      it "tracks the completed step, rolls back in reverse order and halts" do
        organizer = build_organizer(
          build_step(:one),
          cancelling_step,
          build_step(:three)
        )

        result = organizer.call(log: log, cancellation: signal)

        expect(result).to be_cancelled
        expect(result.cancellation_reason).to eq("stop")
        expect(log).to eq([
          :one,
          :two,
          [:two, :rollback],
          [:one, :rollback]
        ])
      end

      it "shares the signal with nested organizers and unwinds around hooks" do
        inner = build_organizer(
          build_step(:inner_one),
          build_step(:inner_two) { context.cancellation.cancel!("stop") },
          build_step(:inner_three)
        )
        outer = build_organizer(build_step(:outer_one), inner, build_step(:outer_three))
        outer.around do |chain|
          context.log << :around_enter
          begin
            chain.call
          ensure
            context.log << :around_exit
          end
        end

        result = outer.call(log: log, cancellation: signal)

        expect(result).to be_cancelled
        expect(result.cancellation_reason).to eq("stop")
        expect(log).to eq([
          :around_enter,
          :outer_one,
          :inner_one,
          :inner_two,
          [:inner_two, :rollback],
          [:inner_one, :rollback],
          [:outer_one, :rollback],
          :around_exit
        ])
      end
    end

    describe "when rollback raises while cancelling" do
      it "raises the rollback error and preserves the cancellation reason" do
        failing_rollback = build_step(:one)
        failing_rollback.send(:define_method, :rollback) do
          context.log << [:one, :rollback]
          raise "rollback failed"
        end
        cancelling_step = build_step(:two) { context.cancellation.cancel!("stop") }
        organizer = build_organizer(failing_rollback, cancelling_step)

        context = Context.new(log: log, cancellation: signal)

        expect {
          organizer.call(context)
        }.to raise_error("rollback failed")
        expect(context).to be_cancelled
        expect(context.cancellation_reason).to eq("stop")
      end
    end

    describe "when an ordinary error and cancellation happen together" do
      it "raises the ordinary error and preserves the cancellation reason" do
        failing_step = build_step(:two) do
          context.cancellation.cancel!("stop")
          raise "boom"
        end
        organizer = build_organizer(
          build_step(:one),
          failing_step,
          build_step(:three)
        )

        context = Context.new(log: log, cancellation: signal)

        expect {
          organizer.call(context)
        }.to raise_error("boom")
        expect(context).to be_cancelled
        expect(context.cancellation_reason).to eq("stop")
        expect(log).to eq([:one, :two, [:one, :rollback]])
      end
    end

    describe "with a fresh signal after a cancelled invocation" do
      it "runs every step normally" do
        original_signal = signal
        cancelling_step = build_step(:two) { original_signal.cancel!("stop") }
        organizer = build_organizer(
          build_step(:one),
          cancelling_step,
          build_step(:three)
        )

        cancelled = organizer.call(log: log, cancellation: signal)
        expect(cancelled).to be_cancelled

        fresh_log = []
        result = organizer.call(log: fresh_log, cancellation: Cancellation.new)

        expect(result).to be_success
        expect(result).not_to be_cancelled
        expect(result.cancellation_reason).to be_nil
        expect(fresh_log).to eq([:one, :two, :three])
      end
    end
  end
end
