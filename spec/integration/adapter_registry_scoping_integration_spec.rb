# frozen_string_literal: true

RSpec.describe 'scoped adapter registry', type: :integration do
  # A minimal request-recording double: it only remembers which HTTP methods
  # were sent through it.
  let(:transport_class) do
    Class.new do
      attr_reader :requests

      def initialize
        @requests = []
      end

      def record(method)
        @requests << method
      end
    end
  end

  let(:left_transport)  { transport_class.new }
  let(:right_transport) { transport_class.new }

  def build_recording_adapter
    Class.new(Faraday::Adapter) do
      def call(env)
        super
        transport = @connection_options.fetch(:transport)
        transport.record(env.method)
        save_response(env, 200, 'ok', {})
      end
    end
  end

  let(:left_adapter)  { build_recording_adapter }
  let(:right_adapter) { build_recording_adapter }

  let(:left_registry) do
    Faraday::AdapterRegistry.new.tap { |registry| registry.set(left_adapter, :custom) }
  end
  let(:right_registry) do
    Faraday::AdapterRegistry.new.tap { |registry| registry.set(right_adapter, :custom) }
  end

  let(:left_client) do
    registry = left_registry
    transport = left_transport
    Faraday.new(nil, adapter_registry: registry) do |builder|
      builder.adapter :custom, transport: transport
    end
  end
  let(:right_client) do
    registry = right_registry
    transport = right_transport
    Faraday.new(nil, adapter_registry: registry) do |builder|
      builder.adapter :custom, transport: transport
    end
  end

  it 'resolves the same adapter name through each local scope' do
    expect(left_client.builder.adapter.klass).to eq(left_adapter)
    expect(right_client.builder.adapter.klass).to eq(right_adapter)
  end

  it 'keeps interleaved calls on their own transports' do
    left_client.get('/a')
    right_client.get('/b')
    left_client.post('/c')
    right_client.get('/d')

    expect(left_transport.requests).to eq(%i[get post])
    expect(right_transport.requests).to eq(%i[get get])
  end

  it 'falls back to the public registration convention outside the scope' do
    public_client = Faraday.new do |builder|
      builder.adapter :test do |stub|
        stub.get('/') { |_| [200, {}, 'public'] }
      end
    end

    expect(public_client.get('/').body).to eq('public')
  end

  it 'falls back when the local scope does not know the name' do
    registry = Faraday::AdapterRegistry.new
    client = Faraday.new(nil, adapter_registry: registry) do |builder|
      builder.adapter :test do |stub|
        stub.get('/') { |_| [200, {}, 'fallback'] }
      end
    end

    expect(client.get('/').body).to eq('fallback')
  end

  it 'isolates registration changes to the scope where they are made' do
    replacement = Class.new(Faraday::Adapter) do
      def call(env)
        super
        save_response(env, 202, 'replaced', {})
      end
    end

    left_registry.set(replacement, :custom)

    # The untouched scope keeps its own binding.
    expect(right_client.builder.adapter.klass).to eq(right_adapter)
    expect(right_client.get('/').body).to eq('ok')
    expect(right_transport.requests).to eq([:get])

    # The changed scope picks up the new registration for fresh lookups.
    expect(left_client.builder.adapter.klass).to eq(replacement)
  end

  it 'keeps an already constructed connection bound after re-registration' do
    left_client.get('/before')

    replacement = Class.new(Faraday::Adapter) do
      def call(env)
        raise 'replacement adapter must not be used'
      end
    end
    left_registry.set(replacement, :custom)

    expect { left_client.get('/after') }.not_to raise_error
    expect(left_transport.requests).to eq(%i[get get])
  end

  it 'does not leak scoped adapters into the process-wide registry' do
    left_client
    right_client

    handler_registry = Faraday::RackBuilder::Handler::REGISTRY
    expect(handler_registry.fetch(:custom)).to be_nil
  end

  it 'keeps the local naming scope when the connection is duplicated' do
    copy = left_client.dup

    expect(copy.builder.adapter_registry).to equal(left_registry)
    expect(copy.builder.adapter.klass).to eq(left_adapter)

    copy.get('/copied')
    expect(left_transport.requests).to eq([:get])
  end
end
