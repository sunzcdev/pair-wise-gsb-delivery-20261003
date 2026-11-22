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

    describe 'the versioned snapshot' do
      let(:snapshot) { subject.marshal_dump }

      it 'is tagged with the supported marshal version and a finished state' do
        expect(snapshot[Faraday::Response::MARSHAL_VERSION_KEY]).to eq(Faraday::Response::MARSHAL_VERSION)
        expect(snapshot[Faraday::Response::MARSHAL_STATE_KEY]).to eq(Faraday::Response::MARSHAL_STATE_FINISHED)
      end

      it 'only carries the response-facing values' do
        expect(snapshot.keys).to contain_exactly(
          Faraday::Response::MARSHAL_VERSION_KEY,
          Faraday::Response::MARSHAL_STATE_KEY,
          :status, :body, :response_headers, :url
        )
      end
    end

    it { expect(loaded.env[:params]).to be_nil }
    it { expect(loaded.env[:body]).to eq(env[:body]) }
    it { expect(loaded.env[:response_headers]).to eq(env[:response_headers]) }
    it { expect(loaded.env[:status]).to eq(env[:status]) }
    it { expect(loaded.env[:url]).to eq(env[:url]) }

    it 'keeps the public readers available after restore' do
      expect(loaded).to be_finished
      expect(loaded.status).to eq(env[:status])
      expect(loaded.body).to eq(env[:body])
      expect(loaded.headers).to eq(env[:response_headers])
      expect(loaded['content-type']).to eq('text/plain')
      expect(loaded.url).to eq(env[:url])
      expect(loaded.success?).to be(false)
      expect(loaded.to_hash).to eq(subject.to_hash)
    end

    it 'drops registered callbacks, in-flight request state and external IO' do
      subject.env[:method] = :post
      subject.env[:request_body] = 'request-secret'
      subject.env[:parallel_manager] = Object.new
      subject.env[:request] = Faraday::RequestOptions.new(on_data: proc { |chunk| chunk })
      subject.on_complete {}

      restored = Marshal.load(Marshal.dump(subject))

      expect(restored.instance_variable_get(:@on_complete_callbacks)).to be_empty
      expect(restored.env[:method]).to be_nil
      expect(restored.env.request_body).to be_nil
      expect(restored.env[:parallel_manager]).to be_nil
      expect(restored.env[:request]).to be_nil
    end

    it 'round-trips a successful response' do
      response = Faraday::Response.new(Faraday::Env.from(
                                         status: 200, body: 'all good',
                                         response_headers: { 'Content-Type' => 'text/plain' },
                                         url: Faraday::Utils.URI('https://example.test/ok')
                                       ))
      restored = Marshal.load(Marshal.dump(response))

      expect(restored).to be_finished
      expect(restored.success?).to be(true)
      expect(restored.status).to eq(200)
      expect(restored.body).to eq('all good')
      expect(restored.headers['Content-Type']).to eq('text/plain')
      expect(restored.url).to eq(URI('https://example.test/ok'))
    end

    it 'round-trips an error response' do
      restored = loaded

      expect(restored).to be_finished
      expect(restored.success?).to be(false)
      expect(restored.status).to eq(404)
      expect(restored.body).to eq('yikes')
      expect(restored.headers['Content-Type']).to eq('text/plain')
      expect(restored.url).to eq(env[:url])
    end

    context 'when the response is not finished' do
      let(:loaded) { Marshal.load(Marshal.dump(Faraday::Response.new)) }

      it 'serializes a versioned, pending snapshot' do
        snapshot = Faraday::Response.new.marshal_dump

        expect(snapshot).to eq(
          Faraday::Response::MARSHAL_VERSION_KEY => Faraday::Response::MARSHAL_VERSION,
          Faraday::Response::MARSHAL_STATE_KEY => Faraday::Response::MARSHAL_STATE_PENDING
        )
      end

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

    context 'when loading data written by an older Faraday version' do
      it 'loads a legacy flat hash of a finished response' do
        legacy = {
          status: 503, body: 'unavailable',
          response_headers: { 'Content-Type' => 'text/plain' },
          url: Faraday::Utils.URI('https://legacy.test/err')
        }
        response = Faraday::Response.allocate
        response.marshal_load(legacy)

        expect(response).to be_finished
        expect(response.status).to eq(503)
        expect(response.body).to eq('unavailable')
        expect(response.headers['Content-Type']).to eq('text/plain')
        expect(response.url).to eq(URI('https://legacy.test/err'))
      end

      it 'loads a legacy nil snapshot of an unfinished response' do
        response = Faraday::Response.allocate
        response.marshal_load(nil)

        expect(response).not_to be_finished
        expect(response.env).to be_nil
        expect(response.instance_variable_get(:@on_complete_callbacks)).to be_empty
      end
    end

    context 'when the snapshot cannot be recognized' do
      def load(snapshot)
        Faraday::Response.allocate.tap { |response| response.marshal_load(snapshot) }
      end

      it 'raises for an unknown snapshot version' do
        expect do
          load(Faraday::Response::MARSHAL_VERSION_KEY => 999,
               Faraday::Response::MARSHAL_STATE_KEY => :finished)
        end.to raise_error(Faraday::SnapshotError, /unsupported .* snapshot version 999/)
      end

      it 'raises for an unknown snapshot state' do
        expect do
          load(Faraday::Response::MARSHAL_VERSION_KEY => Faraday::Response::MARSHAL_VERSION,
               Faraday::Response::MARSHAL_STATE_KEY => :unknown)
        end.to raise_error(Faraday::SnapshotError, /unrecognized .* snapshot state/)
      end

      it 'raises for a snapshot that is not a hash' do
        expect { load('not-a-snapshot') }.to raise_error(Faraday::SnapshotError, /unrecognized .* snapshot/)
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
end
