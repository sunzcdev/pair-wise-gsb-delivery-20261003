# frozen_string_literal: true

HandoffCore = Shoryuken::FailureHandoff

RSpec.describe Shoryuken::FailureHandoff do
  before { Shoryuken::FailureHandoff.reset_recovery_marks! }

  let(:identity) do
    HandoffCore::Identity.new(source_queue: 'orders', source_message_id: 'msg-1')
  end

  describe HandoffCore::Identity do
    it 'builds a stable key from queue and message id only' do
      expect(identity.idempotency_key).to eq('orders/msg-1')
      expect(identity.key).to eq('orders/msg-1')
      expect(identity.to_s).to eq('orders/msg-1')
    end

    it 'keeps the same identity across receipt changes' do
      redelivery = HandoffCore::Identity.new(source_queue: 'orders', source_message_id: 'msg-1')
      other      = HandoffCore::Identity.new(source_queue: 'orders', source_message_id: 'msg-2')

      expect(redelivery).to eq(identity)
      expect(redelivery.idempotency_key).to eq(identity.idempotency_key)
      expect(other.idempotency_key).not_to eq(identity.idempotency_key)
    end
  end

  describe HandoffCore::Result do
    describe '.wrap' do
      it 'passes Result objects through' do
        result = HandoffCore::Result.accepted
        expect(HandoffCore::Result.wrap(result)).to equal(result)
      end

      it 'normalizes booleans' do
        expect(HandoffCore::Result.wrap(true)).to be_accepted
        expect(HandoffCore::Result.wrap(false)).to be_rejected
      end

      it 'normalizes nil to not_found' do
        expect(HandoffCore::Result.wrap(nil)).to be_not_found
      end

      it 'normalizes status tokens' do
        expect(HandoffCore::Result.wrap(:accepted)).to be_accepted
        expect(HandoffCore::Result.wrap('admitted')).to be_accepted
        expect(HandoffCore::Result.wrap('success')).to be_accepted
        expect(HandoffCore::Result.wrap(:rejected)).to be_rejected
        expect(HandoffCore::Result.wrap('denied')).to be_rejected
        expect(HandoffCore::Result.wrap(:not_found)).to be_not_found
        expect(HandoffCore::Result.wrap('missing')).to be_not_found
        expect(HandoffCore::Result.wrap(:weird)).to be_unknown
      end

      it 'normalizes hash status and accepted flags' do
        expect(HandoffCore::Result.wrap(status: 'accepted')).to be_accepted
        expect(HandoffCore::Result.wrap('state' => 'rejected')).to be_rejected
        expect(HandoffCore::Result.wrap(accepted: true)).to be_accepted
        expect(HandoffCore::Result.wrap(accepted: false)).to be_rejected
        expect(HandoffCore::Result.wrap(outcome: 'mystery')).to be_unknown
        expect(HandoffCore::Result.wrap(other: 1)).to be_unknown
      end

      it 'normalizes duck-typed status objects' do
        accepted_duck = Class.new do
          def accepted? = true
          def rejected? = false
        end.new
        rejected_duck = Class.new do
          def accepted? = false
          def rejected? = true
        end.new
        status_duck = Struct.new(:status).new(:accepted)

        expect(HandoffCore::Result.wrap(accepted_duck)).to be_accepted
        expect(HandoffCore::Result.wrap(rejected_duck)).to be_rejected
        expect(HandoffCore::Result.wrap(status_duck)).to be_accepted
        expect(HandoffCore::Result.wrap(Object.new)).to be_unknown
      end

      it 'returns unknown with the error when given one' do
        error = RuntimeError.new('boom')
        result = HandoffCore::Result.wrap(:accepted, error: error)
        expect(result).to be_unknown
        expect(result.error).to eq(error)
      end
    end

    it 'exposes terminal? only for accepted and rejected' do
      expect(HandoffCore::Result.accepted).to be_terminal
      expect(HandoffCore::Result.rejected).to be_terminal
      expect(HandoffCore::Result.unknown).not_to be_terminal
      expect(HandoffCore::Result.not_found).not_to be_terminal
    end
  end

  describe HandoffCore::Dispatcher do
    let(:context) do
      id = HandoffCore::Identity.new(source_queue: 'q', source_message_id: 'm')
      {
        identity: id, key: id.idempotency_key, source_queue: 'q', source_message_id: 'm',
        receipt_handle: 'r', record: nil, payload: {}, exception: nil, body: nil, worker: nil
      }
    end

    def dispatch(adapter, names = HandoffCore::Dispatcher::ACCEPT_METHODS)
      HandoffCore::Dispatcher.new(adapter).call(names, context)
    end

    it 'invokes keyword methods with custom parameter names' do
      adapter = Class.new do
        def admit(source_queue:, message_id:, failure: nil)
          [source_queue, message_id, failure]
        end
      end.new

      status, name, value = dispatch(adapter)
      expect(status).to eq(:ok)
      expect(name).to eq(:admit)
      expect(value).to eq(['q', 'm', nil])
    end

    it 'invokes positional queue/message/payload methods' do
      adapter = Class.new do
        def receive_failure(queue, id, payload)
          [queue, id, payload.class]
        end
      end.new

      status, _name, value = dispatch(adapter)
      expect(status).to eq(:ok)
      expect(value).to eq(['q', 'm', Hash])
    end

    it 'invokes one-argument identity methods' do
      adapter = Class.new do
        def handoff(identity)
          identity.idempotency_key
        end
      end.new

      status, _name, value = dispatch(adapter)
      expect(status).to eq(:ok)
      expect(value).to eq('q/m')
    end

    it 'invokes a callable adapter' do
      adapter = proc { |payload:| payload }
      status, name, value = dispatch(adapter)
      expect(status).to eq(:ok)
      expect(name).to eq(:call)
      expect(value).to eq({})
    end

    it 'returns missing when no candidate method exists' do
      status, = dispatch(Object.new)
      expect(status).to eq(:missing)
    end

    it 'returns error when the invoked method raises' do
      adapter = Class.new do
        def accept_failure(_identity, _record)
          raise 'destination down'
        end
      end.new

      status, name, error = dispatch(adapter)
      expect(status).to eq(:error)
      expect(name).to eq(:accept_failure)
      expect(error.message).to eq('destination down')
    end
  end

  describe HandoffCore::Gateway do
    let(:kwargs) do
      {
        source_queue: 'orders',
        source_message_id: 'msg-1',
        receipt_handle: 'receipt-1',
        exception: ArgumentError.new('bad message')
      }
    end

    def gateway(adapter)
      HandoffCore::Gateway.new(adapter, lookup_first: true)
    end

    it 'returns a prior accepted result without admitting again' do
      adapter = Class.new do
        attr_reader :accepts

        def initialize
          @accepts = 0
        end

        def failure_result(_identity)
          :accepted
        end

        def accept_failure(_identity, _record)
          @accepts += 1
          :accepted
        end
      end.new

      result = gateway(adapter).confirm(**kwargs)

      expect(result).to be_accepted
      expect(adapter.accepts).to eq(0)
    end

    it 'honours a prior rejected result' do
      adapter = Class.new do
        def failure_result(_identity)
          :rejected
        end

        def accept_failure(*)
          raise 'must not admit a known rejection'
        end
      end.new

      expect(gateway(adapter).confirm(**kwargs)).to be_rejected
    end

    it 'admits when no prior result exists and accepts synchronously' do
      adapter = Class.new do
        def failure_result(_identity)
          nil
        end

        def accept_failure(_identity, _record)
          :accepted
        end
      end.new

      expect(gateway(adapter).confirm(**kwargs)).to be_accepted
    end

    it 'treats a normal bare-record return from accept as confirmation' do
      adapter = Class.new do
        def accept_failure(_identity, _record)
          { archived: true, source_message_id: 'msg-1' }
        end
      end.new

      expect(gateway(adapter).confirm(**kwargs)).to be_accepted
    end

    it 'reconciles a lost accept reply through the result query' do
      adapter = Class.new do
        def accept_failure(_identity, _record)
          raise Errno::ECONNRESET
        end

        def failure_result(_identity)
          :accepted
        end
      end.new

      expect(gateway(adapter).confirm(**kwargs)).to be_accepted
    end

    it 'stays unknown when accept is ambiguous and reconciliation is unavailable' do
      adapter = Class.new do
        def accept_failure(_identity, _record)
          raise Errno::ECONNRESET
        end

        def failure_result(_identity)
          raise 'query down'
        end
      end.new

      result = gateway(adapter).confirm(**kwargs)
      expect(result).to be_unknown
      expect(result.error).to be_a(StandardError)
      expect(result.error.message).not_to be_nil
    end

    it 'stays unknown when the adapter exposes no usable methods' do
      result = gateway(Object.new).confirm(**kwargs)
      expect(result).to be_unknown
    end

    it 'accepts when the lookup returns a bare stored record hash' do
      adapter = Class.new do
        def accept_failure(_identity, _record)
          raise Errno::ECONNRESET
        end

        def failure_result(_identity)
          { source_queue: 'orders', source_message_id: 'msg-1', archived: true }
        end
      end.new

      expect(gateway(adapter).confirm(**kwargs)).to be_accepted
    end

    it 'does not treat an empty hash or a pending status as acceptance' do
      empty_adapter = Class.new do
        def failure_result(_identity)
          {}
        end

        def accept_failure(_identity, _record)
          raise Errno::ECONNRESET
        end
      end.new
      expect(gateway(empty_adapter).confirm(**kwargs)).to be_unknown

      pending_adapter = Class.new do
        def failure_result(_identity)
          { status: 'pending' }
        end

        def accept_failure(_identity, _record)
          raise Errno::ECONNRESET
        end
      end.new
      expect(gateway(pending_adapter).confirm(**kwargs)).to be_unknown
    end

    it 'interprets an acceptance predicate: true confirms, false is not-found' do
      yes_adapter = Class.new { def accepted?(_identity); true; end }.new
      expect(gateway(yes_adapter).confirm(**kwargs)).to be_accepted

      no_adapter = Class.new do
        def accepted?(_identity)
          false
        end

        def accept_failure(_identity, _record)
          :accepted
        end
      end.new
      result = gateway(no_adapter).confirm(**kwargs)
      expect(result).to be_accepted
    end
  end

  describe HandoffCore::MemoryDestination do
    it 'stores one record per identity and returns the accepted status' do
      destination = HandoffCore::MemoryDestination.new
      identity = HandoffCore::Identity.new(source_queue: 'q', source_message_id: 'm')

      expect(destination.accept_failure(identity, :record)).to eq(:accepted)
      expect(destination.accept_failure(identity, :record)).to eq(:accepted)
      expect(destination.size).to eq(1)
      expect(destination.failure_result(identity)).to eq(:accepted)
      expect(destination.records.size).to eq(1)
    end

    it 'returns nil for unknown identities' do
      destination = HandoffCore::MemoryDestination.new
      identity = HandoffCore::Identity.new(source_queue: 'q', source_message_id: 'missing')
      expect(destination.failure_result(identity)).to be_nil
    end
  end

  describe '.confirm' do
    it 'uses the configured global adapter' do
      destination = HandoffCore::MemoryDestination.new
      Shoryuken.failure_handoff = destination

      result = described_class.confirm(
        source_queue: 'q',
        source_message_id: 'm'
      )

      expect(result).to be_accepted
      expect(destination.size).to eq(1)
    ensure
      Shoryuken.failure_handoff = nil
    end

    it 'raises when no adapter is available' do
      Shoryuken.failure_handoff = nil
      expect { described_class.confirm(source_queue: 'q', source_message_id: 'm') }
        .to raise_error(ArgumentError, /adapter is required/)
    end
  end
end
