# frozen_string_literal: true

RSpec.describe Faraday::Response do
  subject { Faraday::Response.new(env) }

  let(:env) do
    Faraday::Env.from(status: 404, body: 'yikes', url: Faraday::Utils.URI('https://lostisland.github.io/faraday'),
                      response_headers: { 'Content-Type' => 'text/plain' })
  end

  it { expect(subject.finished?).to be_truthy }
  it { expect { subject.finish({}) }.to raise_error(RuntimeError) }
  it { expect(subject.success?).to be_falsey }
  it { expect(subject.status).to eq(404) }
  it { expect(subject.body).to eq('yikes') }
  it { expect(subject.url).to eq(URI('https://lostisland.github.io/faraday')) }
  it { expect(subject.headers['Content-Type']).to eq('text/plain') }
  it { expect(subject['content-type']).to eq('text/plain') }

  describe '#body' do
    context 'when a nil-status response has distinct request and response bodies' do
      let(:env) { Faraday::Env.from(status: nil, request_body: 'request', response_body: 'response') }

      it { expect(subject.body).to eq('response') }
    end

    context 'when the explicit response body is false' do
      let(:env) { Faraday::Env.from(status: nil, request_body: 'request', response_body: false) }

      it { expect(subject.body).to be(false) }
    end

    context 'when a nil-status response only has the legacy Env body' do
      let(:env) { Faraday::Env.from(status: nil, body: 'legacy-body') }

      it { expect(subject.body).to eq('legacy-body') }
    end
  end

  describe '#apply_request' do
    before { subject.apply_request(body: 'a=b', method: :post) }

    it { expect(subject.body).to eq('yikes') }
    it { expect(subject.env[:method]).to eq(:post) }
  end

  describe '#to_hash' do
    let(:hash) { subject.to_hash }

    it { expect(hash).to be_a(Hash) }
    it { expect(hash[:status]).to eq(subject.status) }
    it { expect(hash[:response_headers]).to eq(subject.headers) }
    it { expect(hash[:body]).to eq(subject.body) }
    it { expect(hash[:url]).to eq(subject.env.url) }

    context 'when response is not finished' do
      subject { Faraday::Response.new.to_hash }

      it { is_expected.to eq({ status: nil, body: nil, response_headers: {}, url: nil }) }
    end
  end

  describe 'marshal serialization support' do
    subject { Faraday::Response.new }
    let(:loaded) { Marshal.load(Marshal.dump(subject)) }

    before do
      subject.on_complete {}
      subject.finish(env.merge(params: 'moo'))
    end

    it { expect(loaded.env[:params]).to be_nil }
    it { expect(loaded.env[:body]).to eq(env[:body]) }
    it { expect(loaded.env[:response_headers]).to eq(env[:response_headers]) }
    it { expect(loaded.env[:status]).to eq(env[:status]) }
    it { expect(loaded.env[:url]).to eq(env[:url]) }

    context 'when the response is not finished' do
      let(:loaded) { Marshal.load(Marshal.dump(Faraday::Response.new)) }

      it { expect(loaded).not_to be_finished }
      it { expect(loaded.env).to be_nil }

      it 'can still register callbacks and be finished' do
        called = false
        loaded.on_complete { called = true }
        loaded.finish(env)

        expect(called).to be(true)
        expect(loaded.status).to eq(404)
      end
    end
  end

  describe '#finish' do
    subject { Faraday::Response.new }

    it 'releases callbacks once they have been called' do
      subject.on_complete {}
      expect(subject.instance_variable_get(:@on_complete_callbacks).size).to eq(1)

      subject.finish(env)

      expect(subject.instance_variable_get(:@on_complete_callbacks)).to be_empty
    end

    it 'calls every callback with the finished env' do
      received = []
      subject.on_complete { |e| received << [:first, e] }
      subject.on_complete { |e| received << [:second, e] }

      subject.finish(env)

      expect(received).to eq([[:first, subject.env], [:second, subject.env]])
    end

    context 'when a callback registers another callback on an unfinished response' do
      let(:trace) { [] }

      before do
        subject.on_complete do
          trace << :first
          subject.on_complete { trace << :from_first }
        end
        subject.on_complete { trace << :second }
        subject.on_complete { trace << :third }
      end

      it 'queues the late callback behind every callback registered before it' do
        subject.finish(env)

        expect(trace).to eq(%i[first second third from_first])
      end

      it 'runs the late callback once, along with the others' do
        subject.finish(env)
        subject.finish(env)

        expect(trace).to eq(%i[first second third from_first])
      end
    end

    context 'when a callback registers another callback while the queue is draining' do
      it 'runs it after the currently running callback returns' do
        trace = []
        subject.on_complete do
          trace << :first
          subject.on_complete { trace << :late }
          trace << :after_registering
        end
        subject.finish(env)

        expect(trace).to eq(%i[first after_registering late])
        expect(subject.body).to eq('yikes')
      end
    end

    context 'when completion is notified more than once' do
      it 'runs the callbacks only on the first notification' do
        trace = []
        subject.on_complete { trace << :called }

        subject.finish(env)
        expect { subject.finish(env) }.not_to raise_error

        expect(trace).to eq([:called])
      end

      it 'keeps the first env and ignores later ones' do
        subject.finish(env)
        subject.finish(status: 500, response_body: 'other')

        expect(subject.status).to eq(404)
        expect(subject.body).to eq('yikes')
      end

      it 'makes callbacks registered afterwards run immediately' do
        subject.finish(env)
        received = nil
        subject.on_complete { |e| received = e }

        expect(received).to be(subject.env)
      end
    end

    context 'when one callback raises' do
      let(:error) { Class.new(StandardError) }

      before do
        @trace = []
        subject.on_complete { @trace << :before }
        subject.on_complete do
          @trace << :raising
          raise error, 'callback failed'
        end
        subject.on_complete { @trace << :after }
      end

      it 'still runs the remaining callbacks and then re-raises the error' do
        expect { subject.finish(env) }.to raise_error(error, 'callback failed')

        expect(@trace).to eq(%i[before raising after])
      end

      it 'keeps the response finished and readable' do
        begin
          subject.finish(env)
        rescue error
          nil
        end

        expect(subject).to be_finished
        expect(subject.status).to eq(404)
        expect(subject.body).to eq('yikes')
        expect(subject.env).to be_a(Faraday::Env)
      end

      it 'does not run a callback twice when completion is notified again' do
        begin
          subject.finish(env)
        rescue error
          nil
        end
        expect { subject.finish(env) }.not_to raise_error

        expect(@trace).to eq(%i[before raising after])
      end

      it 're-raises the first error when several callbacks fail' do
        second_error = Class.new(StandardError)
        subject.on_complete { raise second_error, 'another failure' }

        expect { subject.finish(env) }.to raise_error(error, 'callback failed')
      end
    end

    context 'when a callback notifies completion again' do
      it 'does not run the queue a second time' do
        trace = []
        subject.on_complete do |e|
          trace << :first
          subject.finish(e)
        end
        subject.on_complete { trace << :second }

        subject.finish(env)

        expect(trace).to eq(%i[first second])
      end
    end

    context 'when completion is notified from two threads at once' do
      it 'runs the callbacks only once and reports the same response' do
        trace = []
        started = Queue.new
        release = Queue.new
        subject.on_complete do
          trace << :running
          started << true
          release.pop
        end
        subject.on_complete { trace << :done }

        finisher = Thread.new { subject.finish(env) }
        started.pop

        notifier = Thread.new { subject.finish(env) }
        # The second notification must wait for the first one to finish.
        expect(notifier.join(0.05)).to be_nil

        release << true
        finisher.join
        notifier.join

        expect(trace).to eq(%i[running done])
        expect(subject.status).to eq(404)
        expect(subject.body).to eq('yikes')
      end
    end
  end

  describe '#dup' do
    subject { Faraday::Response.new }

    it 'lets the copy register and finish independently' do
      copy = subject.dup
      trace = []
      subject.on_complete { trace << :original }
      copy.on_complete { trace << :copy }

      copy.finish(env.merge(status: 200, body: 'copy'))
      expect(trace).to eq([:copy])
      expect(subject).not_to be_finished

      subject.finish(env)
      expect(trace).to eq(%i[copy original])
      expect(subject.status).to eq(404)
      expect(copy.status).to eq(200)
    end
  end

  describe '#on_complete' do
    subject { Faraday::Response.new }

    it 'parse body on finish' do
      subject.on_complete { |env| env[:body] = env[:body].upcase }
      subject.finish(env)

      expect(subject.body).to eq('YIKES')
    end

    it 'can access response body in on_complete callback' do
      subject.on_complete { |env| env[:body] = subject.body.upcase }
      subject.finish(env)

      expect(subject.body).to eq('YIKES')
    end

    it 'can access response body in on_complete callback' do
      callback_env = nil
      subject.on_complete { |env| callback_env = env }
      subject.finish({})

      expect(subject.env).to eq(callback_env)
    end
  end

  describe 'deferred completion through an adapter' do
    # An adapter that behaves like an asynchronous one: it returns an unfinished
    # response and lets the spec make the response arrive later. The adapter
    # option array collects the env of every request, and the class keeps track
    # of its instances so a spec can complete the response afterwards.
    let(:deferred_adapter_class) do
      Class.new(Faraday::Adapter) do
        class << self
          attr_accessor :instances
        end
        self.instances = []

        def initialize(app = nil, pending_envs = [], &block)
          super
          self.class.instances << self
          @pending = pending_envs
        end

        def call(env)
          super
          @pending << env
          env.response
        end

        def complete(env = @pending.last, status: 200, body: 'deferred', headers: {})
          save_response(env, status, body, headers, nil, finished: false)
          env.response.finish(env)
        end
      end
    end

    let(:pending) { [] }
    let(:trace) { [] }
    let(:adapter) { deferred_adapter_class.instances.fetch(0) }
    let(:conn) do
      adapter_class = deferred_adapter_class
      pending_envs = pending
      Faraday.new do |builder|
        builder.adapter adapter_class, pending_envs
      end
    end

    it 'returns an unfinished response before the adapter completes it' do
      response = conn.get('/slow')

      expect(response).not_to be_finished
      expect(response.status).to be_nil
      expect(response.body).to be_nil
      expect(pending.size).to eq(1)
      expect(pending.first.response).to be(response)
    end

    it 'runs the callbacks only when the adapter completes the response' do
      response = conn.get('/slow')
      trace << :request_returned
      response.on_complete { trace << :callback }

      expect(trace).to eq([:request_returned])

      adapter.complete

      expect(trace).to eq(%i[request_returned callback])
      expect(response).to be_finished
      expect(response.status).to eq(200)
      expect(response.body).to eq('deferred')
    end

    context 'with middleware on the stack' do
      let(:conn) do
        adapter_class = deferred_adapter_class
        pending_envs = pending
        recording = recording_middleware(trace)
        Faraday.new do |builder|
          builder.use recording
          builder.adapter adapter_class, pending_envs
        end
      end

      def recording_middleware(trace)
        Class.new(Faraday::Middleware) do
          define_method(:on_complete) { |_env| trace << :middleware }
        end
      end

      it 'runs middleware callbacks in registration order with the caller callbacks' do
        response = conn.get('/slow')
        response.on_complete do
          trace << :first_registered
          response.on_complete { trace << :registered_during_finish }
        end
        response.on_complete { trace << :second_registered }

        expect(trace).to be_empty

        adapter.complete

        expect(trace).to eq(%i[middleware first_registered second_registered registered_during_finish])
      end

      it 'does not replay middleware or caller callbacks when completion is repeated' do
        response = conn.get('/slow')
        response.on_complete { trace << :caller }

        adapter.complete
        expect { adapter.complete }.not_to raise_error

        expect(trace).to eq(%i[middleware caller])
        expect(response.body).to eq('deferred')
      end

      it 'keeps the response readable and runs the rest when a caller callback raises' do
        response = conn.get('/slow')
        response.on_complete { trace << :before }
        response.on_complete do
          trace << :raising
          raise 'callback failed'
        end
        response.on_complete { trace << :after }

        expect { adapter.complete }.to raise_error(RuntimeError, 'callback failed')

        expect(trace).to eq(%i[middleware before raising after])
        expect(response).to be_finished
        expect(response.status).to eq(200)
        expect(response.body).to eq('deferred')
        expect(response.env).to be_a(Faraday::Env)
      end
    end
  end
end
