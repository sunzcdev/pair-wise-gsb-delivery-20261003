module Interactor
  describe Organizer do
    include_examples :lint

    let(:organizer) { Class.new.send(:include, Organizer) }

    describe ".organize" do
      let(:interactor2) { double(:interactor2) }
      let(:interactor3) { double(:interactor3) }

      it "sets interactors given class arguments" do
        expect {
          organizer.organize(interactor2, interactor3)
        }.to change {
          organizer.organized
        }.from([]).to([interactor2, interactor3])
      end

      it "sets interactors given an array of classes" do
        expect {
          organizer.organize([interactor2, interactor3])
        }.to change {
          organizer.organized
        }.from([]).to([interactor2, interactor3])
      end
    end

    describe ".organized" do
      it "is empty by default" do
        expect(organizer.organized).to eq([])
      end
    end

    describe ".adapt" do
      let(:interactor2) { double(:interactor2) }

      it "appends an adapter for the interactor with the given mappings" do
        expect {
          organizer.adapt(interactor2,
            inputs: {payer: :account},
            outputs: {receipt: :payment_receipt})
        }.to change {
          organizer.organized
        }.from([]).to([an_instance_of(Interactor::Adapter)])

        adapter = organizer.organized.first
        expect(adapter.interactor_class).to eq(interactor2)
        expect(adapter.inputs).to eq({payer: :account})
        expect(adapter.outputs).to eq({receipt: :payment_receipt})
      end

      it "preserves declaration order alongside organize" do
        interactor3 = double(:interactor3)
        interactor4 = double(:interactor4)

        organizer.organize(interactor2)
        organizer.adapt(interactor3, inputs: {payer: :account})
        organizer.organize(interactor4)

        expect(organizer.organized.size).to eq(3)
        expect(organizer.organized[0]).to eq(interactor2)
        expect(organizer.organized[1]).to be_an_instance_of(Interactor::Adapter)
        expect(organizer.organized[2]).to eq(interactor4)
      end

      it "rejects duplicate output targets at declaration time" do
        expect {
          organizer.adapt(interactor2, outputs: {receipt: :payment, confirmation: :payment})
        }.to raise_error(Interactor::DuplicateOutputError)

        expect(organizer.organized).to eq([])
      end
    end

    describe "adapted failure and rollback" do
      def build_interactor(&block)
        interactor = Class.new.send(:include, Interactor)
        interactor.class_eval(&block) if block
        interactor
      end

      it "fails the organizer and rolls back previously called steps" do
        first = build_interactor do
          def call
            context.steps << :first
          end

          def rollback
            context.steps << :first_rollback
          end
        end
        failing = build_interactor do
          def call
            context.fail!
          end
        end
        never_reached = build_interactor do
          def call
            context.steps << :never_reached
          end
        end

        organizer.organize(first)
        organizer.adapt(failing, inputs: {payer: :account})
        organizer.organize(never_reached)

        result = organizer.call(account: "alice", steps: [])

        expect(result).to be_failure
        expect(result.steps).to eq([:first, :first_rollback])
      end
    end

    describe "#call" do
      let(:instance) { organizer.new }
      let(:context) { double(:context) }
      let(:interactor2) { double(:interactor2) }
      let(:interactor3) { double(:interactor3) }
      let(:interactor4) { double(:interactor4) }

      before do
        allow(instance).to receive(:context) { context }
        allow(organizer).to receive(:organized) {
          [interactor2, interactor3, interactor4]
        }
      end

      it "calls each interactor in order with the context" do
        expect(interactor2).to receive(:call!).once.with(context).ordered
        expect(interactor3).to receive(:call!).once.with(context).ordered
        expect(interactor4).to receive(:call!).once.with(context).ordered

        instance.call
      end
    end
  end
end
