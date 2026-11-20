# frozen_string_literal: true

RSpec.describe Faraday::Adapter::Test do
  let(:stubs) do
    described_class::Stubs.new do |stub|
      stub.get('http://domain.test/hello') do
        [200, { 'Content-Type' => 'text/html' }, 'domain: hello']
      end

      stub.get('http://wrong.test/hello') do
        [200, { 'Content-Type' => 'text/html' }, 'wrong: hello']
      end

      stub.get('http://wrong.test/bait') do
        [404, { 'Content-Type' => 'text/html' }]
      end

      stub.get('/hello') do
        [200, { 'Content-Type' => 'text/html' }, 'hello']
      end

      stub.get('/method-echo') do |env|
        [200, { 'Content-Type' => 'text/html' }, env[:method].to_s]
      end

      stub.get(%r{\A/resources/\d+(?:\?|\z)}) do
        [200, { 'Content-Type' => 'text/html' }, 'show']
      end

      stub.get(%r{\A/resources/(specified)\z}) do |_env, meta|
        [200, { 'Content-Type' => 'text/html' }, "show #{meta[:match_data][1]}"]
      end
    end
  end

  let(:connection) do
    Faraday.new do |builder|
      builder.adapter :test, stubs
    end
  end

  let(:response) { connection.get('/hello') }

  context 'with simple path sets status' do
    subject { response.status }

    it { is_expected.to eq 200 }
  end

  context 'with simple path sets headers' do
    subject { response.headers['Content-Type'] }

    it { is_expected.to eq 'text/html' }
  end

  context 'with simple path sets body' do
    subject { response.body }

    it { is_expected.to eq 'hello' }
  end

  context 'with host points to the right stub' do
    subject { connection.get('http://domain.test/hello').body }

    it { is_expected.to eq 'domain: hello' }
  end

  describe 'can be called several times' do
    subject { connection.get('/hello').body }

    it { is_expected.to eq 'hello' }
  end

  describe 'can handle regular expression path' do
    subject { connection.get('/resources/1').body }

    it { is_expected.to eq 'show' }
  end

  describe 'can handle single parameter block' do
    subject { connection.get('/method-echo').body }

    it { is_expected.to eq 'get' }
  end

  describe 'can handle regular expression path with captured result' do
    subject { connection.get('/resources/specified').body }

    it { is_expected.to eq 'show specified' }
  end

  context 'with get params' do
    subject { connection.get('/param?a=1').body }

    before do
      stubs.get('/param?a=1') { [200, {}, 'a'] }
    end

    it { is_expected.to eq 'a' }
  end

  describe 'ignoring unspecified get params' do
    before do
      stubs.get('/optional?a=1') { [200, {}, 'a'] }
    end

    context 'with multiple params' do
      subject { connection.get('/optional?a=1&b=1').body }

      it { is_expected.to eq 'a' }
    end

    context 'with single param' do
      subject { connection.get('/optional?a=1').body }

      it { is_expected.to eq 'a' }
    end

    context 'without params' do
      subject(:request) { connection.get('/optional') }

      it do
        expect { request }.to raise_error(
          Faraday::Adapter::Test::Stubs::NotFound
        )
      end
    end
  end

  context 'with http headers' do
    before do
      stubs.get('/yo', 'X-HELLO' => 'hello') { [200, {}, 'a'] }
      stubs.get('/yo') { [200, {}, 'b'] }
    end

    context 'with header' do
      subject do
        connection.get('/yo') { |env| env.headers['X-HELLO'] = 'hello' }.body
      end

      it { is_expected.to eq 'a' }
    end

    context 'without header' do
      subject do
        connection.get('/yo').body
      end

      it { is_expected.to eq 'b' }
    end
  end

  describe 'different outcomes for the same request' do
    def make_request
      connection.get('/foo')
    end

    subject(:request) { make_request.body }

    before do
      stubs.get('/foo') { [200, { 'Content-Type' => 'text/html' }, 'hello'] }
      stubs.get('/foo') { [200, { 'Content-Type' => 'text/html' }, 'world'] }
    end

    context 'the first request' do
      it { is_expected.to eq 'hello' }
    end

    context 'the second request' do
      before do
        make_request
      end

      it { is_expected.to eq 'world' }
    end
  end

  describe 'yielding env to stubs' do
    subject { connection.get('http://foo.com/foo?a=1').body }

    before do
      stubs.get '/foo' do |env|
        expect(env[:url].path).to eq '/foo'
        expect(env[:url].host).to eq 'foo.com'
        expect(env[:params]['a']).to eq '1'
        expect(env[:request_headers]['Accept']).to eq 'text/plain'
        [200, {}, 'a']
      end

      connection.headers['Accept'] = 'text/plain'
    end

    it { is_expected.to eq 'a' }
  end

  describe 'params parsing' do
    subject { connection.get('http://foo.com/foo?a[b]=1').body }

    context 'with default encoder' do
      before do
        stubs.get '/foo' do |env|
          expect(env[:params]['a']['b']).to eq '1'
          [200, {}, 'a']
        end
      end

      it { is_expected.to eq 'a' }
    end

    context 'with nested encoder' do
      before do
        stubs.get '/foo' do |env|
          expect(env[:params]['a']['b']).to eq '1'
          [200, {}, 'a']
        end

        connection.options.params_encoder = Faraday::NestedParamsEncoder
      end

      it { is_expected.to eq 'a' }
    end

    context 'with flat encoder' do
      before do
        stubs.get '/foo' do |env|
          expect(env[:params]['a[b]']).to eq '1'
          [200, {}, 'a']
        end

        connection.options.params_encoder = Faraday::FlatParamsEncoder
      end

      it { is_expected.to eq 'a' }
    end
  end

  describe 'raising an error if no stub was found' do
    describe 'for request' do
      subject(:request) { connection.get('/invalid') { [200, {}, []] } }

      it { expect { request }.to raise_error described_class::Stubs::NotFound }
    end

    describe 'for specified host' do
      subject(:request) { connection.get('http://domain.test/bait') }

      it { expect { request }.to raise_error described_class::Stubs::NotFound }
    end

    describe 'for request without specified header' do
      subject(:request) { connection.get('/yo') }

      before do
        stubs.get('/yo', 'X-HELLO' => 'hello') { [200, {}, 'a'] }
      end

      it { expect { request }.to raise_error described_class::Stubs::NotFound }
    end
  end

  describe 'for request with non default params encoder' do
    let(:connection) do
      Faraday.new(request: { params_encoder: Faraday::FlatParamsEncoder }) do |builder|
        builder.adapter :test, stubs
      end
    end
    let(:stubs) do
      described_class::Stubs.new do |stubs|
        stubs.get('/path?a=x&a=y&a=z') { [200, {}, 'a'] }
      end
    end

    context 'when all flat param values are correctly set' do
      subject(:request) { connection.get('/path?a=x&a=y&a=z') }

      it { expect(request.status).to eq 200 }
    end

    shared_examples 'raise NotFound when params do not satisfy the flat param values' do |params|
      subject(:request) { connection.get('/path', params) }

      context "with #{params.inspect}" do
        it { expect { request }.to raise_error described_class::Stubs::NotFound }
      end
    end

    it_behaves_like 'raise NotFound when params do not satisfy the flat param values', { a: %w[x] }
    it_behaves_like 'raise NotFound when params do not satisfy the flat param values', { a: %w[x y] }
    it_behaves_like 'raise NotFound when params do not satisfy the flat param values', { a: %w[x z y] } # NOTE: The order of the value is also compared.
    it_behaves_like 'raise NotFound when params do not satisfy the flat param values', { b: %w[x y z] }
  end

  describe 'strict_mode' do
    let(:stubs) do
      described_class::Stubs.new(strict_mode: true) do |stubs|
        stubs.get('/strict?a=12&b=xy', 'Authorization' => 'Bearer m_ck', 'X-C' => 'hello') { [200, {}, 'a'] }
        stubs.get('/with_user_agent?a=12&b=xy', authorization: 'Bearer m_ck', 'User-Agent' => 'My Agent') { [200, {}, 'a'] }
      end
    end

    context 'when params and headers are exactly set' do
      subject(:request) { connection.get('/strict', { a: '12', b: 'xy' }, { authorization: 'Bearer m_ck', x_c: 'hello' }) }

      it { expect(request.status).to eq 200 }
    end

    context 'when params and headers are exactly set with a custom user agent' do
      subject(:request) { connection.get('/with_user_agent', { a: '12', b: 'xy' }, { authorization: 'Bearer m_ck', 'User-Agent' => 'My Agent' }) }

      it { expect(request.status).to eq 200 }
    end

    shared_examples 'raise NotFound when params do not satisfy the strict check' do |params|
      subject(:request) { connection.get('/strict', params, { 'Authorization' => 'Bearer m_ck', 'X-C' => 'hello' }) }

      context "with #{params.inspect}" do
        it { expect { request }.to raise_error described_class::Stubs::NotFound }
      end
    end

    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { a: '12' }
    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { b: 'xy' }
    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { a: '123', b: 'xy' }
    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { a: '12', b: 'xyz' }
    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { a: '12', b: 'xy', c: 'hello' }
    it_behaves_like 'raise NotFound when params do not satisfy the strict check', { additional: 'special', a: '12', b: 'xy', c: 'hello' }

    shared_examples 'raise NotFound when headers do not satisfy the strict check' do |path, headers|
      subject(:request) { connection.get(path, { a: 12, b: 'xy' }, headers) }

      context "with #{headers.inspect}" do
        it { expect { request }.to raise_error described_class::Stubs::NotFound }
      end
    end

    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/strict', { 'X-C' => 'hello' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck', 'x-c': 'Hi' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/strict', { authorization: 'Basic m_ck', 'x-c': 'hello' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck', 'x-c': 'hello', x_special: 'special' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/with_user_agent', { authorization: 'Bearer m_ck' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/with_user_agent', { authorization: 'Bearer m_ck', user_agent: 'Unknown' }
    it_behaves_like 'raise NotFound when headers do not satisfy the strict check', '/with_user_agent', { authorization: 'Bearer m_ck', user_agent: 'My Agent', x_special: 'special' }

    context 'when strict_mode is disabled' do
      before do
        stubs.strict_mode = false
      end

      shared_examples 'does not raise NotFound even when params do not satisfy the strict check' do |params|
        subject(:request) { connection.get('/strict', params, { 'Authorization' => 'Bearer m_ck', 'X-C' => 'hello' }) }

        context "with #{params.inspect}" do
          it { expect(request.status).to eq 200 }
        end
      end

      it_behaves_like 'does not raise NotFound even when params do not satisfy the strict check', { a: '12', b: 'xy' }
      it_behaves_like 'does not raise NotFound even when params do not satisfy the strict check', { a: '12', b: 'xy', c: 'hello' }
      it_behaves_like 'does not raise NotFound even when params do not satisfy the strict check', { additional: 'special', a: '12', b: 'xy', c: 'hello' }

      shared_examples 'does not raise NotFound even when headers do not satisfy the strict check' do |path, headers|
        subject(:request) { connection.get(path, { a: 12, b: 'xy' }, headers) }

        context "with #{headers.inspect}" do
          it { expect(request.status).to eq 200 }
        end
      end

      it_behaves_like 'does not raise NotFound even when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck', 'x-c': 'hello' }
      it_behaves_like 'does not raise NotFound even when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck', 'x-c': 'hello', x_special: 'special' }
      it_behaves_like 'does not raise NotFound even when headers do not satisfy the strict check', '/strict', { authorization: 'Bearer m_ck', 'x-c': 'hello', user_agent: 'Special Agent' }
      it_behaves_like 'does not raise NotFound even when headers do not satisfy the strict check', '/with_user_agent', { authorization: 'Bearer m_ck', user_agent: 'My Agent' }
      it_behaves_like 'does not raise NotFound even when headers do not satisfy the strict check', '/with_user_agent', { authorization: 'Bearer m_ck', user_agent: 'My Agent', x_special: 'special' }
    end

    describe 'body_match?' do
      let(:stubs) do
        described_class::Stubs.new do |stubs|
          stubs.post('/no_check') { [200, {}, 'ok'] }
          stubs.post('/with_string', 'abc') { [200, {}, 'ok'] }
          stubs.post(
            '/with_proc',
            ->(request_body) { JSON.parse(request_body, symbolize_names: true) == { x: '!', a: [{ m: [{ a: true }], n: 123 }] } },
            { content_type: 'application/json' }
          ) do
            [200, {}, 'ok']
          end
        end
      end

      context 'when trying without any args for body' do
        subject(:without_body) { connection.post('/no_check') }

        it { expect(without_body.status).to eq 200 }
      end

      context 'when trying with string body stubs' do
        subject(:with_string) { connection.post('/with_string', 'abc') }

        it { expect(with_string.status).to eq 200 }
      end

      context 'when trying with proc body stubs' do
        subject(:with_proc) do
          connection.post('/with_proc', JSON.dump(a: [{ n: 123, m: [{ a: true }] }], x: '!'), { 'Content-Type' => 'application/json' })
        end

        it { expect(with_proc.status).to eq 200 }
      end
    end
  end

  describe 'request timeout' do
    subject(:request) do
      connection.get('/sleep') do |req|
        req.options.timeout = timeout
      end
    end

    before do
      stubs.get('/sleep') do
        sleep(0.01)
        [200, {}, '']
      end
    end

    context 'when request is within timeout' do
      let(:timeout) { 1 }

      it { expect(request.status).to eq 200 }
    end

    context 'when request is too slow' do
      let(:timeout) { 0.001 }

      it 'raises an exception' do
        expect { request }.to raise_error(Faraday::TimeoutError)
      end
    end
  end

  describe '#clear' do
    it 'removes pending stubs' do
      stubs.clear
      expect { connection.get('/hello') }.to raise_error(described_class::Stubs::NotFound)
    end

    it 'removes already consumed stubs' do
      expect(connection.get('/hello').body).to eq('hello')
      stubs.clear
      expect { connection.get('/hello') }.to raise_error(described_class::Stubs::NotFound)
    end

    it 'leaves the stubs empty' do
      stubs.clear
      expect(stubs).to be_empty
    end
  end

  describe Faraday::Adapter::Test::Stream do
    # Middleware that records whether the response reached the completed stage.
    class CompletionTracker < Faraday::Middleware
      def initialize(app, sink)
        super(app)
        @sink = sink
      end

      def on_complete(_env)
        @sink.call
      end
    end

    let(:adapter_class) { Faraday::Adapter::Test }
    let(:stream_stubs) { adapter_class::Stubs.new }
    let(:stream_connection) do
      Faraday.new do |builder|
        builder.adapter :test, stream_stubs
      end
    end

    # The same consumer is used for every stub: it records each chunk and keeps
    # the running cumulative length reported by Env's on_data callback.
    let(:received) { [] }
    let(:consumer) do
      proc do |chunk, cumulative_size, _env|
        received << [chunk, cumulative_size]
      end
    end

    def request(path)
      stream_connection.get(path) do |req|
        req.options.on_data = consumer
      end
    end

    let(:headers) { { 'Content-Type' => 'text/plain' } }

    describe 'driving Env streaming callbacks' do
      let!(:chunked) do
        stream = adapter_class::Stream.new(200, headers, %w[hel lo world])
        stream_stubs.get('/chunked') { stream }
        stream
      end

      let!(:empty) do
        stream = adapter_class::Stream.new(200, headers, [])
        stream_stubs.get('/empty') { stream }
        stream
      end

      let!(:broken) do
        error = Faraday::ConnectionFailed.new('boom mid-stream')
        stream = adapter_class::Stream.new(200, headers, ['first', error, 'never'])
        stream_stubs.get('/broken') { stream }
        stream
      end

      it 'delivers every chunk in order with cumulative byte sizes' do
        response = request('/chunked')

        expect(received).to eq([
                                 ['hel', 3],
                                 ['lo', 5],
                                 ['world', 10]
                               ])
        # Streaming responses finish with an empty aggregated body.
        expect(response.body).to eq('')
        expect(response.status).to eq(200)
        expect(response.headers['Content-Type']).to eq('text/plain')

        expect(chunked.delivered).to eq(%w[hel lo world])
        expect(chunked.chunks_delivered).to eq(3)
        expect(chunked.bytes_delivered).to eq(10)
        expect(chunked).to be_finished
        expect(chunked.error).to be_nil
      end

      it 'observes an empty stream as a single empty chunk at size zero' do
        response = request('/empty')

        expect(received).to eq([['', 0]])
        expect(response.body).to eq('')
        expect(response.status).to eq(200)
        expect(response.headers['Content-Type']).to eq('text/plain')

        expect(empty.delivered).to eq([])
        expect(empty.chunks_delivered).to eq(0)
        expect(empty.bytes_delivered).to eq(0)
        expect(empty).to be_finished
        expect(empty.error).to be_nil
      end

      it 'breaks mid-stream and keeps the earlier chunks observable' do
        expect { request('/broken') }.to raise_error(Faraday::ConnectionFailed, 'boom mid-stream')

        # The consumer only sees the chunks delivered before the break.
        expect(received).to eq([['first', 5]])

        expect(broken.delivered).to eq(['first'])
        expect(broken.chunks_delivered).to eq(1)
        expect(broken.bytes_delivered).to eq(5)
        expect(broken).not_to be_finished
        expect(broken.error).to be_a(Faraday::ConnectionFailed)
        expect(broken.error.message).to eq('boom mid-stream')
      end

      it 'does not finish the response or fire on_complete when the stream breaks' do
        completed = false
        connection_with_callback = Faraday.new do |builder|
          builder.use CompletionTracker, -> { completed = true }
          builder.adapter :test, stream_stubs
        end

        expect do
          connection_with_callback.get('/broken') do |req|
            req.options.on_data = consumer
          end
        end.to raise_error(Faraday::ConnectionFailed)

        expect(completed).to be(false)
      end
    end

    describe 'without an on_data callback' do
      it 'concatenates the chunks into a normal response body' do
        stream_stubs.get('/plain') do
          adapter_class::Stream.new(201, headers, %w[ab cd])
        end

        response = stream_connection.get('/plain')

        expect(response.status).to eq(201)
        expect(response.headers['Content-Type']).to eq('text/plain')
        expect(response.body).to eq('abcd')
      end
    end
  end
end
