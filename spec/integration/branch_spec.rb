# frozen_string_literal: true

RSpec.describe "branch steps" do
  include Dry::Monads[:result]

  let(:log) { Test::LOG }

  let(:container) {
    {
      prepare: -> input {
        Test::LOG << :prepare
        Success(input)
      },
      select_route: -> input {
        Test::LOG << :select_route
        input[:route]
      },
      generate_license: -> input {
        Test::LOG << :generate_license
        Success(input.merge(license: "LIC-1"))
      },
      deliver_download: -> input {
        Test::LOG << :deliver_download
        if input[:undeliverable]
          Failure("cannot deliver")
        else
          Success(input.merge(delivered: true))
        end
      },
      pack_parcel: -> input {
        Test::LOG << :pack_parcel
        Success(input.merge(packed: true))
      },
      finalize: -> input {
        Test::LOG << :finalize
        Success(input)
      }
    }
  }

  let(:transaction_class) do
    Class.new do
      include Dry::Transaction(container: Test::BranchContainer)

      step :prepare, with: :prepare

      branch :route, with: :select_route do
        on :digital, join: -> value { value.merge(joined: :digital) } do
          step :generate_license, with: :generate_license
          step :deliver_download, with: :deliver_download
        end

        on :physical do
          step :pack_parcel, with: :pack_parcel
        end
      end

      step :finalize, with: :finalize
    end
  end

  let(:subscriber) do
    Class.new do
      attr_reader :started, :succeeded, :failed

      def initialize
        @started = []
        @succeeded = []
        @failed = []
      end

      def on_step(event)
        started << event[:step_name]
      end

      def on_step_succeeded(event)
        succeeded << event[:step_name]
      end

      def on_step_failed(event)
        failed << event[:step_name]
      end
    end.new
  end

  before do
    Test::LOG = []
    Test::BranchContainer = container
  end

  context "selecting the deeper branch" do
    it "runs the branch's sub-steps in order and joins via the branch's explicit mapping" do
      result = transaction_class.new.call(route: :digital)

      expect(result).to be_success
      expect(log).to eq %i[prepare select_route generate_license deliver_download finalize]
      expect(result.value!).to eq(
        route: :digital, license: "LIC-1", delivered: true, joined: :digital
      )
    end

    it "publishes events for the branch step and its sub-steps in execution order" do
      transaction = transaction_class.new
      transaction.subscribe(subscriber)
      transaction.call(route: :digital)

      expect(subscriber.succeeded).to eq %i[
        prepare generate_license deliver_download route finalize
      ]
      expect(subscriber.failed).to be_empty
    end

    it "does not run the unselected branch or publish events for its steps" do
      transaction = transaction_class.new
      transaction.subscribe(subscriber)
      transaction.call(route: :digital)

      expect(log).not_to include(:pack_parcel)
      expect(subscriber.started).not_to include(:pack_parcel)
      expect(subscriber.succeeded).not_to include(:pack_parcel)
    end
  end

  context "selecting the shallower branch" do
    it "runs only that branch's sub-steps and passes its result through by default" do
      result = transaction_class.new.call(route: :physical)

      expect(result).to be_success
      expect(log).to eq %i[prepare select_route pack_parcel finalize]
      expect(result.value!).to eq(route: :physical, packed: true)
    end

    it "publishes success events for the selected branch only" do
      transaction = transaction_class.new
      transaction.subscribe(subscriber)
      transaction.call(route: :physical)

      expect(subscriber.succeeded).to eq %i[prepare pack_parcel route finalize]
      expect(subscriber.succeeded).not_to include(:generate_license, :deliver_download)
      expect(log).not_to include(:generate_license, :deliver_download)
    end
  end

  context "when the selector returns an unknown branch" do
    it "raises a dedicated error" do
      expect { transaction_class.new.call(route: :express) }.to raise_error(
        Dry::Transaction::UnknownBranchError,
        "step +`route`+ selected unknown branch +`express`+"
      )
    end
  end

  context "when a sub-step within a branch fails" do
    it "returns the failure and preserves the full branch path" do
      result = transaction_class.new.stack.(Success(route: :digital, undeliverable: true))

      expect(result).to be_failure

      failure = result.failure
      expect(failure).to be_a(Dry::Transaction::StepFailure)
      expect(failure.step.name).to eq :deliver_download
      expect(failure.path).to eq %i[route deliver_download]
      expect(failure.value).to eq "cannot deliver"
    end

    it "unwraps the failure value from #call and matches on the failing sub-step" do
      matched = nil

      result = transaction_class.new.call(route: :digital, undeliverable: true) do |m|
        m.success { :unexpected }
        m.failure(:deliver_download) { |value| matched = value }
        m.failure { :other_failure }
      end

      expect(result).to eq "cannot deliver"
      expect(matched).to eq "cannot deliver"
      expect(transaction_class.new.call(route: :digital, undeliverable: true)).to eql(
        Failure("cannot deliver")
      )
    end

    it "publishes a failure event for the failing sub-step" do
      transaction = transaction_class.new
      transaction.subscribe(subscriber)
      transaction.call(route: :digital, undeliverable: true)

      expect(subscriber.failed).to eq %i[deliver_download]
      expect(subscriber.succeeded).to eq %i[prepare generate_license]
    end
  end

  describe "around steps" do
    let(:container) {
      {
        select_route: -> input { input[:route] },
        generate_license: -> input {
          Test::LOG << :generate_license
          Success(input)
        },
        deliver_download: -> input {
          Test::LOG << :deliver_download
          Success(input)
        },
        pack_parcel: -> input {
          Test::LOG << :pack_parcel
          Success(input)
        },
        wrap_digital: -> input, &block {
          Test::LOG << :digital_enter
          result = block.(Success(input))
          Test::LOG << :digital_exit
          result
        },
        instrument: -> input, &block {
          Test::LOG << :around_enter
          result = block.(Success(input))
          Test::LOG << :around_exit
          result
        }
      }
    }

    let(:transaction_class) do
      Class.new do
        include Dry::Transaction(container: Test::BranchContainer)

        around :instrument, with: :instrument

        branch :route, with: :select_route do
          on :digital do
            around :wrap_digital, with: :wrap_digital
            step :generate_license, with: :generate_license
            step :deliver_download, with: :deliver_download
          end

          on :physical do
            step :pack_parcel, with: :pack_parcel
          end
        end
      end
    end

    it "propagates enter/exit semantics through and within branches" do
      result = transaction_class.new.call(route: :digital)

      expect(result).to be_success
      expect(log).to eq %i[
        around_enter
        digital_enter generate_license deliver_download digital_exit
        around_exit
      ]
    end

    it "wraps the selected branch only" do
      transaction_class.new.call(route: :physical)

      expect(log).to eq %i[around_enter pack_parcel around_exit]
    end
  end

  describe "with an inline selector" do
    let(:transaction_class) do
      Class.new do
        include Dry::Transaction(container: Test::BranchContainer)

        branch :route, selector: -> input { input[:route] } do
          on :digital do
            step :generate_license, with: :generate_license
          end

          on :physical do
            step :pack_parcel, with: :pack_parcel
          end
        end
      end
    end

    it "selects the branch using the given callable" do
      result = transaction_class.new.call(route: :physical)

      expect(result).to be_success
      expect(log).to eq %i[pack_parcel]
    end
  end
end
