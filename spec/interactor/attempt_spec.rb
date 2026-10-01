module Interactor
  describe Attempt do
    let(:build_step) {
      ->(tag) {
        Class.new do
          include Interactor

          define_method(:call) {
            context.ran ||= []
            context.ran << tag
          }

          define_method(:rollback) {
            context.rolled_back ||= []
            context.rolled_back << tag
          }
        end
      }
    }

    let(:main_one) { build_step.call(:main_one) }
    let(:main_two) { build_step.call(:main_two) }

    let(:failing_optional) {
      Class.new do
        include Interactor

        def call
          context.ran ||= []
          context.ran << :failing_optional
          context.fail!(message: "supplement failed")
        end
      end
    }

    describe "explicit business failure" do
      it "keeps the main flow running and records the reason" do
        optional_one = build_step.call(:optional_one)
        optional_two = build_step.call(:optional_two)
        failing = failing_optional
        m1 = main_one
        m2 = main_two

        organizer = Class.new do
          include Interactor::Organizer

          organize m1

          attempt optional_one, failing, optional_two do |failure|
            context.supplement_reason = failure.message
          end

          organize m2
        end

        result = organizer.call

        expect(result).to be_a_success
        expect(result.ran).to eq([:main_one, :optional_one, :failing_optional, :main_two])
        expect(result.supplement_reason).to eq("supplement failed")
      end

      it "rolls back only the steps completed inside the subflow" do
        optional_one = build_step.call(:optional_one)
        optional_two = build_step.call(:optional_two)
        failing = failing_optional
        m1 = main_one
        m2 = main_two

        organizer = Class.new do
          include Interactor::Organizer

          organize m1
          attempt optional_one, failing, optional_two do |_|
          end
          organize m2
        end

        result = organizer.call

        expect(result.rolled_back).to eq([:optional_one])
      end

      it "discards the subflow's top-level key additions, removals and reassignments" do
        main = Class.new do
          include Interactor

          def call
            context.kept = "original"
            context.tampered = "outer"
          end
        end

        sub = Class.new do
          include Interactor

          def call
            context.added_by_subflow = true
            context.tampered = "subflow"
            context.fail!(message: "boom")
          end
        end

        organizer = Class.new do
          include Interactor::Organizer

          organize main
          attempt(sub) { |_| }
        end

        result = organizer.call

        expect(result).to be_a_success
        expect(result.kept).to eq("original")
        expect(result.tampered).to eq("outer")
        expect(result.to_h.key?(:added_by_subflow)).to be(false)
      end

      it "restores keys the subflow removed" do
        main = Class.new do
          include Interactor

          def call
            context.present = "still here"
          end
        end

        sub = Class.new do
          include Interactor

          def call
            context.delete_field(:present)
            context.fail!
          end
        end

        organizer = Class.new do
          include Interactor::Organizer

          organize main
          attempt(sub) { |_| }
        end

        result = organizer.call

        expect(result).to be_a_success
        expect(result.present).to eq("still here")
      end
    end

    describe "subflow success followed by outer failure" do
      it "joins the subflow steps into the outer reverse rollback" do
        optional_one = build_step.call(:optional_one)
        optional_two = build_step.call(:optional_two)
        failing_main = Class.new do
          include Interactor

          def call
            context.fail!(message: "outer failed")
          end
        end
        m1 = main_one
        o1 = optional_one
        o2 = optional_two
        fm = failing_main

        organizer = Class.new do
          include Interactor::Organizer

          organize m1
          attempt o1, o2
          organize fm
        end

        result = organizer.call

        expect(result).to be_a_failure
        expect(result.ran).to eq([:main_one, :optional_one, :optional_two])
        expect(result.rolled_back).to eq([:optional_two, :optional_one, :main_one])
      end

      it "gives each completed instance exactly one rollback position" do
        repeat = Class.new do
          include Interactor

          def call
            context.ran_ids ||= []
            context.ran_ids << object_id
          end

          def rollback
            context.rolled_ids ||= []
            context.rolled_ids << object_id
          end
        end
        failing_main = Class.new do
          include Interactor

          def call
            context.fail!
          end
        end
        rp = repeat
        fm = failing_main

        organizer = Class.new do
          include Interactor::Organizer

          organize rp
          attempt rp, rp
          organize rp
          attempt rp
          organize fm
        end

        result = organizer.call

        expect(result.ran_ids.length).to eq(5)
        expect(result.ran_ids.uniq.length).to eq(5)
        expect(result.rolled_ids.sort).to eq(result.ran_ids.sort)
      end
    end

    describe "two levels of nesting" do
      it "rolls back only the innermost completed steps on an inner failure" do
        inner_one = build_step.call(:inner_one)
        inner_two = build_step.call(:inner_two)
        failing_inner = Class.new do
          include Interactor

          def call
            context.ran ||= []
            context.ran << :failing_inner
            context.fail!(message: "inner boom")
          end
        end

        i1 = inner_one
        i2 = inner_two
        fi = failing_inner

        # The inner organizer runs its whole supplement as a single recoverable
        # subflow nested inside the outer organizer's recoverable subflow.
        inner = Class.new do
          include Interactor::Organizer

          attempt i1, i2, fi do |failure|
            context.inner_reason = failure.message
          end
        end

        outer_one = build_step.call(:outer_one)
        outer_two = build_step.call(:outer_two)
        o1 = outer_one
        o2 = outer_two
        inn = inner

        outer = Class.new do
          include Interactor::Organizer

          organize o1
          attempt inn
          organize o2
        end

        result = outer.call

        expect(result).to be_a_success
        expect(result.ran).to eq([:outer_one, :inner_one, :inner_two, :failing_inner, :outer_two])
        expect(result.rolled_back).to eq([:inner_two, :inner_one])
        expect(result.inner_reason).to eq("inner boom")
      end

      it "rolls back committed inner steps, then outer steps, when the outer fails" do
        inner_one = build_step.call(:inner_one)
        inner_two = build_step.call(:inner_two)
        i1 = inner_one
        i2 = inner_two
        inner = Class.new do
          include Interactor::Organizer

          organize i1, i2
        end

        outer_one = build_step.call(:outer_one)
        failing_outer = Class.new do
          include Interactor

          def call
            context.fail!
          end
        end
        o1 = outer_one
        inn = inner
        fo = failing_outer

        outer = Class.new do
          include Interactor::Organizer

          organize o1
          attempt inn
          organize fo
        end

        result = outer.call

        expect(result.ran).to eq([:outer_one, :inner_one, :inner_two])
        expect(result.rolled_back).to eq([:inner_two, :inner_one, :outer_one])
      end
    end

    describe "the same Interactor class appearing more than once" do
      it "gives each completed instance exactly one rollback position" do
        repeat = Class.new do
          include Interactor

          def call
            context.ran_ids ||= []
            context.ran_ids << object_id
          end

          def rollback
            context.rolled_ids ||= []
            context.rolled_ids << object_id
          end
        end
        failing = Class.new do
          include Interactor

          def call
            context.fail!
          end
        end
        rp = repeat
        fl = failing

        organizer = Class.new do
          include Interactor::Organizer

          organize rp
          attempt rp, rp
          organize fl
        end

        result = organizer.call

        expect(result.ran_ids.length).to eq(3)
        expect(result.ran_ids.uniq.length).to eq(3)
        expect(result.rolled_ids.sort).to eq(result.ran_ids.sort)
      end
    end

    describe "a failing recovery block" do
      it "propagates a business failure raised in the recovery block" do
        main = main_one
        successful = build_step.call(:successful)
        failing_optional = Class.new do
          include Interactor

          def call
            context.fail!(message: "subflow")
          end
        end

        organizer = Class.new do
          include Interactor::Organizer

          organize main

          attempt failing_optional do |_|
            context.recovery_notes = "recorded"
            context.fail!(message: "recovery failed")
          end

          organize successful
        end

        result = organizer.call

        expect(result).to be_a_failure
        expect(result.message).to eq("recovery failed")
        expect(result.recovery_notes).to eq("recorded")
        expect(result.rolled_back).to eq([:main_one])
      end

      it "propagates an ordinary exception raised in the recovery block" do
        failing_optional = Class.new do
          include Interactor

          def call
            context.fail!
          end
        end

        organizer = Class.new do
          include Interactor::Organizer

          attempt(failing_optional) { |_| raise "recovery boom" }
        end

        expect {
          organizer.call
        }.to raise_error(RuntimeError, "recovery boom")
      end
    end

    describe "an ordinary exception inside the subflow" do
      it "propagates instead of being treated as a business failure" do
        explosive = Class.new do
          include Interactor

          def call
            raise "kaboom"
          end
        end

        organizer = Class.new do
          include Interactor::Organizer

          attempt(explosive) { |_| context.never = true }
        end

        expect {
          organizer.call
        }.to raise_error(RuntimeError, "kaboom")
      end

      it "rolls back the whole flow and skips remaining steps" do
        ran = []
        rolled = []

        main = Class.new do
          include Interactor

          define_method(:call) { ran << :main }
          define_method(:rollback) { rolled << :main }
        end
        sub_completed = Class.new do
          include Interactor

          define_method(:call) { ran << :sub_completed }
          define_method(:rollback) { rolled << :sub_completed }
        end
        explosive = Class.new do
          include Interactor

          define_method(:call) { raise "kaboom" }
        end
        never = Class.new do
          include Interactor

          define_method(:call) { ran << :never }
        end

        organizer = Class.new do
          include Interactor::Organizer

          organize main
          attempt sub_completed, explosive do |_|
          end
          organize never
        end

        expect { organizer.call }.to raise_error(RuntimeError, "kaboom")

        expect(ran).to eq([:main, :sub_completed])
        expect(rolled).to eq([:sub_completed, :main])
      end

      it "rolls back the whole flow through two nested subflows" do
        ran = []
        rolled = []

        outer_step = Class.new do
          include Interactor

          define_method(:call) { ran << :outer_step }
          define_method(:rollback) { rolled << :outer_step }
        end
        inner_step = Class.new do
          include Interactor

          define_method(:call) { ran << :inner_step }
          define_method(:rollback) { rolled << :inner_step }
        end
        explosive = Class.new do
          include Interactor

          define_method(:call) { raise "deep kaboom" }
        end

        inner = Class.new do
          include Interactor::Organizer

          organize inner_step
          attempt explosive do |_|
          end
        end
        inn = inner
        os = outer_step

        outer = Class.new do
          include Interactor::Organizer

          organize os
          attempt inn
        end

        expect { outer.call }.to raise_error(RuntimeError, "deep kaboom")

        expect(ran).to eq([:outer_step, :inner_step])
        expect(rolled).to eq([:inner_step, :outer_step])
      end
    end

    describe "imperative entry point" do
      it "recovers inside a plain interactor's call" do
        optional = build_step.call(:optional)
        failing_optional = Class.new do
          include Interactor

          def call
            context.fail!(message: "nope")
          end
        end
        opt = optional
        flop = failing_optional

        host = Class.new do
          include Interactor

          define_method(:call) {
            context.before = true
            attempt(opt, flop) do |failure|
              context.why = failure.message
            end
            context.after = true
          }
        end

        result = host.call

        expect(result).to be_a_success
        expect(result.before).to be(true)
        expect(result.after).to be(true)
        expect(result.why).to eq("nope")
        expect(result.rolled_back).to eq([:optional])
      end

      it "supports a symbol recovery method" do
        failing_optional = Class.new do
          include Interactor

          def call
            context.fail!(message: "symbol")
          end
        end
        flop = failing_optional

        host = Class.new do
          include Interactor

          define_method(:call) {
            attempt(flop, recovery: :note_failure)
          }

          define_method(:note_failure) { |failure|
            context.why = failure.message
          }
        end

        result = host.call

        expect(result).to be_a_success
        expect(result.why).to eq("symbol")
      end
    end
  end
end
