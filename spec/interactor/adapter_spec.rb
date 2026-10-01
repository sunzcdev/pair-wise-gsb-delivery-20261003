require "spec_helper"

describe Interactor::Adapter do
  def build_interactor(&block)
    interactor = Class.new.send(:include, Interactor)
    interactor.class_eval(&block) if block
    interactor
  end

  def build_organizer(&block)
    Class.new.send(:include, Interactor::Organizer).tap do |organizer|
      organizer.class_eval(&block) if block
    end
  end

  def adapt(interactor, inputs: {}, outputs: {})
    Class.new.send(:include, Interactor::Organizer).adapt(
      interactor,
      inputs: inputs,
      outputs: outputs
    )
  end

  describe ".adapt" do
    it "returns an Interactor class carrying the mappings" do
      interactor = build_interactor
      organizer = build_organizer

      step = organizer.adapt(
        interactor,
        inputs: {account: :payer},
        outputs: {receipt: :payment_receipt}
      )

      expect(step.ancestors).to include(Interactor)
      expect(step.adapted_interactor).to eq(interactor)
      expect(step.input_mapping).to eq(account: :payer)
      expect(step.output_mapping).to eq(receipt: :payment_receipt)
    end

    it "defaults to empty mappings and stringifies keys into symbols" do
      interactor = build_interactor
      organizer = build_organizer

      step = organizer.adapt(
        interactor,
        inputs: {"account" => "payer"},
        outputs: {"receipt" => "payment_receipt"}
      )

      expect(step.input_mapping).to eq(account: :payer)
      expect(step.output_mapping).to eq(receipt: :payment_receipt)
    end

    it "appends the adapted step to the organized interactors" do
      interactor1 = build_interactor
      interactor2 = build_interactor
      organizer = build_organizer do
        organize interactor1, adapt(interactor2)
      end

      expect(organizer.organized.first).to eq(interactor1)
      expect(organizer.organized.last.adapted_interactor).to eq(interactor2)
    end

    it "supports adapted steps declared inside an array" do
      interactor = build_interactor
      organizer = build_organizer do
        organize [adapt(interactor)]
      end

      expect(organizer.organized.last.adapted_interactor).to eq(interactor)
    end
  end

  describe "input mapping" do
    let(:seen_inputs) { [] }

    let(:create_receipt) {
      inputs = seen_inputs
      build_interactor do
        define_method(:call) do
          inputs << context.to_h
          context.receipt = "receipt-for-#{context.account}"
        end
      end
    }

    let(:organizer) {
      step = adapt(
        create_receipt,
        inputs: {account: :payer},
        outputs: {receipt: :payment_receipt}
      )
      build_organizer { organize step }
    }

    it "feeds mapped parent keys into the local context" do
      organizer.call(payer: "alice")

      expect(seen_inputs).to eq([{account: "alice"}])
    end

    it "does not leak unmapped parent keys into the local context" do
      organizer.call(payer: "alice", unrelated: "secret")

      expect(seen_inputs.first).to eq(account: "alice")
    end

    it "does not leak temporary local keys back into the parent context" do
      result = organizer.call(payer: "alice")

      expect(result.to_h).to eq(
        payer: "alice",
        payment_receipt: "receipt-for-alice"
      )
    end

    it "raises MissingInput when a required parent key is absent" do
      expect {
        organizer.call
      }.to raise_error(Interactor::MissingInput, /:payer.*:account/)

      expect(seen_inputs).to be_empty
    end

    it "treats an explicitly nil parent key as present" do
      expect {
        organizer.call(payer: nil)
      }.not_to raise_error

      expect(seen_inputs).to eq([{account: nil}])
    end
  end

  describe "output mapping" do
    it "writes mapped local keys back into the parent context" do
      interactor = build_interactor do
        def call
          context.receipt = "R-1"
        end
      end
      organizer = build_organizer do
        organize adapt(
          interactor,
          inputs: {account: :payer},
          outputs: {receipt: :payment_receipt}
        )
      end

      result = organizer.call(payer: "alice")

      expect(result.payment_receipt).to eq("R-1")
      expect(result.to_h).not_to have_key(:receipt)
    end

    it "raises MissingOutput when a declared output is absent" do
      interactor = build_interactor do
        def call
          context.one = 1
        end
      end
      organizer = build_organizer do
        organize adapt(interactor, outputs: {one: :one, two: :two})
      end

      expect {
        organizer.call
      }.to raise_error(Interactor::MissingOutput, /:two/)
    end

    it "verifies every output before merging any of them" do
      interactor = build_interactor do
        def call
          context.one = 1
        end
      end
      organizer = build_organizer do
        organize adapt(interactor, outputs: {one: :one, two: :two})
      end

      context = Interactor::Context.new
      expect {
        organizer.call(context)
      }.to raise_error(Interactor::MissingOutput)

      expect(context.to_h).not_to have_key(:one)
    end

    it "accepts a nil output as a present output" do
      interactor = build_interactor do
        def call
          context.receipt = nil
        end
      end
      organizer = build_organizer do
        organize adapt(interactor, outputs: {receipt: :payment_receipt})
      end

      result = organizer.call

      expect(result.to_h).to have_key(:payment_receipt)
      expect(result.payment_receipt).to be_nil
    end

    it "rejects output mappings targeting the same parent key before running" do
      ran = false
      interactor = build_interactor do
        define_method(:call) { ran = true }
      end
      organizer = build_organizer do
        organize adapt(interactor, outputs: {one: :shared, two: :shared})
      end

      expect {
        organizer.call
      }.to raise_error(ArgumentError, /shared/)
      expect(ran).to be(false)
    end
  end

  describe "failure and rollback" do
    let(:events) { [] }

    let(:create_receipt) {
      event_log = events
      build_interactor do
        define_method(:call) do
          event_log << [:call, context.account, context.object_id]
          context.receipt = "R-#{context.account}"
        end

        define_method(:rollback) do
          event_log << [
            :rollback,
            context.account,
            context.receipt,
            context.object_id
          ]
        end
      end
    }

    let(:failing_interactor) {
      build_interactor do
        def call
          context.fail!(internal_failure: true)
        end
      end
    }

    it "fails the parent context but keeps local failure keys isolated" do
      step = adapt(failing_interactor)
      organizer = build_organizer do
        organize step
      end

      result = organizer.call

      expect(result).to be_failure
      expect(result.to_h).not_to have_key(:internal_failure)
    end

    it "raises Failure for call! while preserving the parent context" do
      step = adapt(failing_interactor)
      organizer = build_organizer do
        organize step
      end

      expect {
        organizer.call!
      }.to raise_error(Interactor::Failure) { |error|
        expect(error.context).to be_a(Interactor::Context)
      }
    end

    it "rolls back the instance that ran with its local context" do
      step = adapt(
        create_receipt,
        inputs: {account: :payer},
        outputs: {receipt: :payment_receipt}
      )
      fail_step = adapt(failing_interactor)
      organizer = build_organizer { organize step, fail_step }

      organizer.call(payer: "alice")

      call_event, rollback_event = events
      expect(call_event[0..1]).to eq([:call, "alice"])
      expect(rollback_event[0..2]).to eq([:rollback, "alice", "R-alice"])
      expect(call_event.last).to eq(rollback_event.last)
    end

    it "keeps separate rollback records for two calls to the same class" do
      first = adapt(
        create_receipt,
        inputs: {account: :payer},
        outputs: {receipt: :payment_receipt}
      )
      second = adapt(
        create_receipt,
        inputs: {account: :buyer},
        outputs: {receipt: :buyer_receipt}
      )
      fail_step = adapt(failing_interactor)
      organizer = build_organizer do
        organize first, second, fail_step
      end

      result = organizer.call(payer: "alice", buyer: "bob")

      projected = events.map { |name, account, third|
        name == :call ? [name, account] : [name, account, third]
      }
      expect(projected).to eq(
        [
          [:call, "alice"],
          [:call, "bob"],
          [:rollback, "bob", "R-bob"],
          [:rollback, "alice", "R-alice"]
        ]
      )
      expect(events[0].last).to eq(events[3].last)
      expect(events[1].last).to eq(events[2].last)
      expect(events[0].last).not_to eq(events[1].last)
      expect(result._called.count { |called|
        called.class.adapted_interactor == create_receipt
      }).to eq(2)
    end

    it "preserves the wrapped interactor's hooks and single-instance record" do
      hook_log = []
      hooked = build_interactor do
        before { hook_log << :before }
        after { hook_log << :after }

        define_method(:call) do
          hook_log << [:call, context._called.size]
          context.receipt = "R"
        end

        define_method(:rollback) do
          hook_log << [:rollback, context._called.size]
        end
      end
      first = adapt(hooked, inputs: {account: :payer}, outputs: {receipt: :r1})
      second = adapt(hooked, inputs: {account: :buyer}, outputs: {receipt: :r2})
      fail_step = adapt(failing_interactor)
      organizer = build_organizer { organize first, second, fail_step }

      organizer.call(payer: "a", buyer: "b")

      expect(hook_log).to eq(
        [
          :before,
          [:call, 0],
          :after,
          :before,
          [:call, 0],
          :after,
          [:rollback, 1],
          [:rollback, 1]
        ]
      )
    end

    it "rolls back local side effects when a declared output is missing" do
      rollback_log = []
      incomplete = Class.new.send(:include, Interactor)
      incomplete.class_eval do
        define_method(:call) do
          context.one = 1
        end

        define_method(:rollback) do
          rollback_log << context.one
        end
      end
      incomplete_step = adapt(incomplete, outputs: {one: :one, two: :two})
      organizer = build_organizer do
        organize incomplete_step
      end

      expect {
        organizer.call
      }.to raise_error(Interactor::MissingOutput)

      expect(rollback_log).to eq([1])
    end

    it "does not track a failed adapted step for rollback" do
      step = adapt(failing_interactor)
      organizer = build_organizer { organize step }

      result = organizer.call

      expect(result._called).to be_empty
    end
  end

  describe "within an organizer" do
    it "passes the parent context between plain and adapted steps" do
      load_account = build_interactor do
        def call
          context.payer = "account-#{context.account_id}"
        end
      end
      create_receipt = build_interactor do
        def call
          context.receipt = "R-#{context.account}"
        end
      end
      send_email = build_interactor do
        def call
          context.emailed_to = context.payment_receipt
        end
      end
      organizer = build_organizer do
        organize(
          load_account,
          adapt(
            create_receipt,
            inputs: {account: :payer},
            outputs: {receipt: :payment_receipt}
          ),
          send_email
        )
      end

      result = organizer.call(account_id: 7)

      expect(result).to be_success
      expect(result.payer).to eq("account-7")
      expect(result.payment_receipt).to eq("R-account-7")
      expect(result.emailed_to).to eq("R-account-7")
      expect(result.to_h).not_to have_key(:account)
      expect(result.to_h).not_to have_key(:receipt)
    end

    it "leaves plain organize declarations working without migration" do
      interactor1 = build_interactor do
        def call
          context.value = "one"
        end
      end
      interactor2 = build_interactor do
        def call
          context.value = context.value.upcase
        end
      end
      organizer = build_organizer do
        organize interactor1, interactor2
      end

      result = organizer.call

      expect(result).to be_success
      expect(result.value).to eq("ONE")
    end
  end
end
