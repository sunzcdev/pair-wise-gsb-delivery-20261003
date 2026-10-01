module Interactor
  describe Adapter do
    def build_interactor(&block)
      interactor = Class.new.send(:include, Interactor)
      interactor.class_eval(&block) if block
      interactor
    end

    def build_organizer(&block)
      organizer = Class.new.send(:include, Interactor::Organizer)
      organizer.class_eval(&block) if block
      organizer
    end

    let(:interactor) { build_interactor }

    describe ".new" do
      it "accepts string keys and normalizes them to symbols" do
        adapter = Adapter.new(interactor,
          inputs: {"payer" => "account"},
          outputs: {"receipt" => "payment_receipt"})

        expect(adapter.inputs).to eq({payer: :account})
        expect(adapter.outputs).to eq({receipt: :payment_receipt})
      end

      it "rejects duplicate output targets before any invocation" do
        expect {
          Adapter.new(interactor, outputs: {receipt: :payment, confirmation: :payment})
        }.to raise_error(DuplicateOutputError, /`payment`/)
      end
    end

    describe "#call!" do
      let(:parent_context) { Context.new(account: "alice", secret: "shh") }

      context "with input mapping" do
        let(:interactor) {
          build_interactor do
            def call
              context.observed_payer = context.payer
              context.observed_secret = context.to_h.key?(:secret)
            end
          end
        }

        it "provides declared inputs under the interactor's own keys" do
          adapter = Adapter.new(interactor,
            inputs: {payer: :account},
            outputs: {observed_payer: :seen})

          adapter.call!(parent_context)

          expect(parent_context.seen).to eq("alice")
        end

        it "does not leak undeclared parent keys into the local context" do
          adapter = Adapter.new(interactor,
            inputs: {payer: :account},
            outputs: {observed_secret: :leaked})

          adapter.call!(parent_context)

          expect(parent_context.leaked).to eq(false)
        end

        it "distinguishes a nil input from a missing input" do
          parent_context = Context.new(account: nil)
          adapter = Adapter.new(interactor,
            inputs: {payer: :account},
            outputs: {observed_payer: :seen})

          adapter.call!(parent_context)

          expect(parent_context.to_h.key?(:seen)).to eq(true)
          expect(parent_context.seen).to be_nil
        end

        it "raises a missing input error when a declared input is absent" do
          adapter = Adapter.new(interactor, inputs: {payer: :account})
          parent_context = Context.new

          expect {
            adapter.call!(parent_context)
          }.to raise_error(MissingInputError, /`account`.*`payer`/)
        end
      end

      context "with output mapping" do
        let(:interactor) {
          build_interactor do
            def call
              context.receipt = "receipt-for-#{context.payer}"
              context.scratch = "temporary"
            end
          end
        }

        it "merges declared outputs into the parent context" do
          adapter = Adapter.new(interactor,
            inputs: {payer: :account},
            outputs: {receipt: :payment_receipt})

          adapter.call!(parent_context)

          expect(parent_context.payment_receipt).to eq("receipt-for-alice")
        end

        it "does not leak undeclared local keys into the parent context" do
          adapter = Adapter.new(interactor,
            inputs: {payer: :account},
            outputs: {receipt: :payment_receipt})

          adapter.call!(parent_context)

          expect(parent_context.to_h.key?(:scratch)).to eq(false)
          expect(parent_context.to_h.key?(:receipt)).to eq(false)
        end

        it "distinguishes a nil output from a missing output" do
          interactor = build_interactor do
            def call
              context.receipt = nil
            end
          end
          adapter = Adapter.new(interactor, outputs: {receipt: :payment_receipt})

          adapter.call!(parent_context)

          expect(parent_context.to_h.key?(:payment_receipt)).to eq(true)
          expect(parent_context.payment_receipt).to be_nil
        end

        it "raises a missing output error when a declared output is absent" do
          interactor = build_interactor
          adapter = Adapter.new(interactor, outputs: {receipt: :payment_receipt})

          expect {
            adapter.call!(parent_context)
          }.to raise_error(MissingOutputError, /`receipt`.*`payment_receipt`/)
        end

        it "verifies all outputs before merging any of them" do
          interactor = build_interactor do
            def call
              context.receipt = "receipt"
            end
          end
          adapter = Adapter.new(interactor,
            outputs: {receipt: :payment_receipt, confirmation: :payment_confirmation})

          expect {
            adapter.call!(parent_context)
          }.to raise_error(MissingOutputError)

          expect(parent_context.to_h.key?(:payment_receipt)).to eq(false)
          expect(parent_context.to_h.key?(:payment_confirmation)).to eq(false)
        end
      end

      context "when the interactor fails" do
        let(:interactor) {
          build_interactor do
            def call
              context.fail!(error: "boom")
            end
          end
        }

        it "fails the parent context" do
          adapter = Adapter.new(interactor, inputs: {payer: :account})

          expect {
            adapter.call!(parent_context)
          }.to raise_error(Failure)

          expect(parent_context).to be_failure
        end

        it "does not track the failed instance for rollback" do
          adapter = Adapter.new(interactor, inputs: {payer: :account})

          expect {
            adapter.call!(parent_context)
          }.to raise_error(Failure)

          expect(parent_context._called).to eq([])
        end
      end

      context "rollback" do
        let(:interactor) {
          build_interactor do
            def call
              context.receipt = "receipt-for-#{context.payer}"
            end

            def rollback
              context.log << "rolled-back-#{context.payer}"
            end
          end
        }

        it "tracks the executed instance so rollback sees the local context" do
          parent_context = Context.new(account: "alice", log: [])
          adapter = Adapter.new(interactor,
            inputs: {payer: :account, log: :log},
            outputs: {receipt: :payment_receipt})

          adapter.call!(parent_context)
          parent_context.rollback!

          expect(parent_context.log).to eq(["rolled-back-alice"])
        end

        it "rolls back two adapters of the same class as separate records" do
          organizer = build_organizer
          organizer.adapt interactor,
            inputs: {payer: :account_one, log: :log},
            outputs: {receipt: :receipt_one}
          organizer.adapt interactor,
            inputs: {payer: :account_two, log: :log},
            outputs: {receipt: :receipt_two}

          result = organizer.call(account_one: "alice", account_two: "bob", log: [])

          expect(result).to be_success
          expect(result._called.first(2).map { |instance| instance.context.payer }).to eq(["alice", "bob"])

          result.rollback!

          expect(result.log).to eq(["rolled-back-bob", "rolled-back-alice"])
        end
      end
    end
  end
end
