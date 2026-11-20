# frozen_string_literal: true

RSpec.describe Faraday::Response::RaiseError do
  let(:conn) do
    Faraday.new do |b|
      b.response :raise_error
      b.adapter :test do |stub|
        stub.get('ok') { [200, { 'Content-Type' => 'text/html' }, '<body></body>'] }
        stub.get('bad-request') { [400, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('unauthorized') { [401, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('forbidden') { [403, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('not-found') { [404, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('proxy-error') { [407, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('request-timeout') { [408, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('conflict') { [409, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('unprocessable-content') { [422, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('too-many-requests') { [429, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('4xx') { [499, { 'X-Reason' => 'because' }, 'keep looking'] }
        stub.get('nil-status') { [nil, { 'X-Reason' => 'nil' }, 'fail'] }
        stub.get('server-error') { [500, { 'X-Error' => 'bailout' }, 'fail'] }
      end
    end
  end

  it 'raises no exception for 200 responses' do
    expect { conn.get('ok') }.not_to raise_error
  end

  it 'raises Faraday::BadRequestError for 400 responses' do
    expect { conn.get('bad-request') }.to raise_error(Faraday::BadRequestError) do |ex|
      expect(ex.message).to eq('the server responded with status 400 for GET http:/bad-request')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(400)
      expect(ex.response_status).to eq(400)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::UnauthorizedError for 401 responses' do
    expect { conn.get('unauthorized') }.to raise_error(Faraday::UnauthorizedError) do |ex|
      expect(ex.message).to eq('the server responded with status 401 for GET http:/unauthorized')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(401)
      expect(ex.response_status).to eq(401)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::ForbiddenError for 403 responses' do
    expect { conn.get('forbidden') }.to raise_error(Faraday::ForbiddenError) do |ex|
      expect(ex.message).to eq('the server responded with status 403 for GET http:/forbidden')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(403)
      expect(ex.response_status).to eq(403)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::ResourceNotFound for 404 responses' do
    expect { conn.get('not-found') }.to raise_error(Faraday::ResourceNotFound) do |ex|
      expect(ex.message).to eq('the server responded with status 404 for GET http:/not-found')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(404)
      expect(ex.response_status).to eq(404)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::ProxyAuthError for 407 responses' do
    expect { conn.get('proxy-error') }.to raise_error(Faraday::ProxyAuthError) do |ex|
      expect(ex.message).to eq('407 "Proxy Authentication Required"')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(407)
      expect(ex.response_status).to eq(407)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::RequestTimeoutError for 408 responses' do
    expect { conn.get('request-timeout') }.to raise_error(Faraday::RequestTimeoutError) do |ex|
      expect(ex.message).to eq('the server responded with status 408 for GET http:/request-timeout')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(408)
      expect(ex.response_status).to eq(408)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::ConflictError for 409 responses' do
    expect { conn.get('conflict') }.to raise_error(Faraday::ConflictError) do |ex|
      expect(ex.message).to eq('the server responded with status 409 for GET http:/conflict')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(409)
      expect(ex.response_status).to eq(409)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises legacy Faraday::UnprocessableEntityError for 422 responses' do
    expect { conn.get('unprocessable-content') }.to raise_error(Faraday::UnprocessableEntityError) do |ex|
      expect(ex.message).to eq('the server responded with status 422 for GET http:/unprocessable-content')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(422)
      expect(ex.response_status).to eq(422)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::UnprocessableContentError for 422 responses' do
    expect { conn.get('unprocessable-content') }.to raise_error(Faraday::UnprocessableContentError) do |ex|
      expect(ex.message).to eq('the server responded with status 422 for GET http:/unprocessable-content')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(422)
      expect(ex.response_status).to eq(422)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::TooManyRequestsError for 429 responses' do
    expect { conn.get('too-many-requests') }.to raise_error(Faraday::TooManyRequestsError) do |ex|
      expect(ex.message).to eq('the server responded with status 429 for GET http:/too-many-requests')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(429)
      expect(ex.response_status).to eq(429)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::NilStatusError for nil status in response' do
    expect { conn.get('nil-status') }.to raise_error(Faraday::NilStatusError) do |ex|
      expect(ex.message).to eq('http status could not be derived from the server response')
      expect(ex.response[:headers]['X-Reason']).to eq('nil')
      expect(ex.response[:status]).to be_nil
      expect(ex.response_status).to be_nil
      expect(ex.response_body).to eq('fail')
      expect(ex.response_headers['X-Reason']).to eq('nil')
    end
  end

  it 'raises Faraday::ClientError for other 4xx responses' do
    expect { conn.get('4xx') }.to raise_error(Faraday::ClientError) do |ex|
      expect(ex.message).to eq('the server responded with status 499 for GET http:/4xx')
      expect(ex.response[:headers]['X-Reason']).to eq('because')
      expect(ex.response[:status]).to eq(499)
      expect(ex.response_status).to eq(499)
      expect(ex.response_body).to eq('keep looking')
      expect(ex.response_headers['X-Reason']).to eq('because')
    end
  end

  it 'raises Faraday::ServerError for 500 responses' do
    expect { conn.get('server-error') }.to raise_error(Faraday::ServerError) do |ex|
      expect(ex.message).to eq('the server responded with status 500 for GET http:/server-error')
      expect(ex.response[:headers]['X-Error']).to eq('bailout')
      expect(ex.response[:status]).to eq(500)
      expect(ex.response_status).to eq(500)
      expect(ex.response_body).to eq('fail')
      expect(ex.response_headers['X-Error']).to eq('bailout')
    end
  end

  describe 'request info' do
    let(:conn) do
      Faraday.new do |b|
        b.response :raise_error, **middleware_options
        b.adapter :test do |stub|
          stub.post(url, request_body, request_headers) do
            [400, { 'X-Reason' => 'because' }, 'keep looking']
          end
        end
      end
    end
    let(:middleware_options) { {} }
    let(:request_body) { JSON.generate({ 'item' => 'sth' }) }
    let(:request_headers) { { 'Authorization' => 'Basic 123' } }
    let(:url_path) { 'request' }
    let(:query_params) { 'full=true' }
    let(:url) { "#{url_path}?#{query_params}" }

    subject(:perform_request) do
      conn.post url do |req|
        req.headers['Authorization'] = 'Basic 123'
        req.body = request_body
      end
    end

    it 'returns the request info in the exception' do
      expect { perform_request }.to raise_error(Faraday::BadRequestError) do |ex|
        expect(ex.response[:request][:method]).to eq(:post)
        expect(ex.response[:request][:url]).to eq(URI("http:/#{url}"))
        expect(ex.response[:request][:url_path]).to eq("/#{url_path}")
        expect(ex.response[:request][:params]).to eq({ 'full' => 'true' })
        expect(ex.response[:request][:headers]).to match(a_hash_including(request_headers))
        expect(ex.response[:request][:body]).to eq(request_body)
      end
    end

    describe 'DEFAULT_OPTION: include_request' do
      before(:each) do
        Faraday::Response::RaiseError.instance_variable_set(:@default_options, nil)
        Faraday::Middleware.instance_variable_set(:@default_options, nil)
      end

      after(:all) do
        Faraday::Response::RaiseError.instance_variable_set(:@default_options, nil)
        Faraday::Middleware.instance_variable_set(:@default_options, nil)
      end

      context 'when RaiseError DEFAULT_OPTION (include_request: true) is used' do
        it 'includes request info in the exception' do
          expect { perform_request }.to raise_error(Faraday::BadRequestError) do |ex|
            expect(ex.response.keys).to contain_exactly(
              :status,
              :headers,
              :body,
              :request
            )
          end
        end
      end

      context 'when application sets default_options `include_request: false`' do
        before(:each) do
          Faraday::Response::RaiseError.default_options = { include_request: false }
        end

        context 'and when include_request option is omitted' do
          it 'does not include request info in the exception' do
            expect { perform_request }.to raise_error(Faraday::BadRequestError) do |ex|
              expect(ex.response.keys).to contain_exactly(
                :status,
                :headers,
                :body
              )
            end
          end
        end

        context 'and when include_request option is explicitly set for instance' do
          let(:middleware_options) { { include_request: true } }

          it 'includes request info in the exception' do
            expect { perform_request }.to raise_error(Faraday::BadRequestError) do |ex|
              expect(ex.response.keys).to contain_exactly(
                :status,
                :headers,
                :body,
                :request
              )
            end
          end
        end
      end
    end
  end

  describe 'two-stage response view' do
    let(:raw_body) { '{"error":"boom"}' }
    let(:parsed_body) { { 'error' => 'boom' } }
    let(:response_headers) { { 'Content-Type' => 'application/json', 'X-Raw' => 'yes' } }

    let(:conn) do
      Faraday.new do |b|
        stack_order.call(b)
        b.adapter :test do |stub|
          stub.get('boom') { [500, response_headers, raw_body] }
        end
      end
    end

    # Middleware complete responses in reverse registration order, so the last
    # registered middleware sees the adapter-fresh response first.
    shared_examples 'a request without the two-stage view' do
      it 'keeps the legacy response reader and leaves the stages empty' do
        expect { conn.get('boom') }.to raise_error(Faraday::ServerError) do |ex|
          expect(ex.response_stages).to be_nil
          expect(ex.raw_response).to be_nil
          expect(ex.adapter_response).to be_nil
          expect(ex.current_response).to be_nil
          expect(ex.response).not_to have_key(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        end
      end
    end

    context 'when RaiseError is positioned before the transformation in the stack' do
      let(:stack_order) do
        lambda do |b|
          b.response :raise_error, response_stages: response_stages
          b.response :json
        end
      end

      context 'with response_stages enabled' do
        let(:response_stages) { true }

        it 'raises after Json parsed the body, but keeps the raw adapter stage' do
          expect { conn.get('boom') }.to raise_error(Faraday::ServerError) do |ex|
            # request info is still available on the raised error
            expect(ex.response[:request][:method]).to eq(:get)
            expect(ex.response[:request][:url]).to eq(URI('http:/boom'))

            # the legacy reader shows the transformed (parsed) response
            expect(ex.response[:status]).to eq(500)
            expect(ex.response_body).to eq(parsed_body)

            # the adapter stage still holds the untouched string body
            expect(ex.raw_response[:status]).to eq(500)
            expect(ex.raw_response[:headers]).to eq(response_headers)
            expect(ex.raw_response[:body]).to eq(raw_body)
            expect(ex.raw_response[:body]).to be_a(String)
            expect(ex.adapter_response).to eq(ex.raw_response)

            # the current stage reflects the Json transformation
            expect(ex.current_response).to equal(ex.response)
            expect(ex.current_response[:body]).to eq(parsed_body)
            expect(ex.current_response[:headers]).to eq(response_headers)

            # the private transport key never leaks into the stored response
            expect(ex.response).not_to have_key(Faraday::RAW_ADAPTER_RESPONSE_KEY)
          end
        end

        it 'captures an independent snapshot that later mutations do not affect' do
          captured_raw = nil
          begin
            conn.get('boom')
          rescue Faraday::ServerError => ex
            captured_raw = ex.raw_response.dup
            ex.response[:body] = { 'tampered' => true }
          end

          expect(captured_raw[:body]).to eq(raw_body)
        end
      end

      context 'with response_stages disabled (default)' do
        let(:response_stages) { false }

        include_examples 'a request without the two-stage view'
      end
    end

    context 'when the transformation is positioned before RaiseError in the stack' do
      let(:stack_order) do
        lambda do |b|
          b.response :json
          b.response :raise_error, response_stages: response_stages
        end
      end

      context 'with response_stages enabled' do
        let(:response_stages) { true }

        it 'raises before Json runs, so both stages hold the raw adapter response' do
          expect { conn.get('boom') }.to raise_error(Faraday::ServerError) do |ex|
            # request info is still available on the raised error
            expect(ex.response[:request][:method]).to eq(:get)
            expect(ex.response[:request][:url]).to eq(URI('http:/boom'))

            # the legacy reader shows the untransformed adapter response
            expect(ex.response[:status]).to eq(500)
            expect(ex.response_body).to eq(raw_body)

            # both stages carry the adapter-fresh, unparsed response
            expect(ex.raw_response[:status]).to eq(500)
            expect(ex.raw_response[:headers]).to eq(response_headers)
            expect(ex.raw_response[:body]).to eq(raw_body)
            expect(ex.raw_response[:body]).to be_a(String)
            expect(ex.adapter_response).to eq(ex.raw_response)
            expect(ex.current_response).to equal(ex.response)
            expect(ex.current_response[:body]).to eq(raw_body)
          end
        end
      end

      context 'with response_stages disabled (default)' do
        let(:response_stages) { false }

        include_examples 'a request without the two-stage view'
      end
    end

    context 'when include_request is false' do
      let(:conn) do
        Faraday.new do |b|
          b.response :json
          b.response :raise_error, include_request: false, response_stages: true
          b.adapter :test do |stub|
            stub.get('boom?token=SECRET') { [500, response_headers, raw_body] }
          end
        end
      end

      it 'omits request info from every stage and the message while keeping both responses' do
        expect { conn.get('boom?token=SECRET') }.to raise_error(Faraday::ServerError) do |ex|
          expect(ex.response.keys).to contain_exactly(:status, :headers, :body)
          expect(ex.message).to include('method and url are not available')
          expect(ex.inspect).not_to include('SECRET')

          expect(ex.raw_response).to eq(
            status: 500,
            headers: response_headers,
            body: raw_body
          )
          expect(ex.current_response.keys).to contain_exactly(:status, :headers, :body)
          expect(ex.current_response[:body]).to eq(raw_body)
        end
      end
    end

    context 'when another middleware raises before RaiseError' do
      let(:conn) do
        Faraday.new do |b|
          # Json is registered last, so it completes first and raises while
          # RaiseError (outer) still has its on_complete callback pending.
          b.response :raise_error, **middleware_options
          b.response :json
          b.adapter :test do |stub|
            stub.post('invalid-json') { [200, { 'Content-Type' => 'application/json' }, 'not-json'] }
          end
        end
      end
      let(:middleware_options) { { response_stages: true } }

      it 'attaches the two stages to the ParsingError as well' do
        expect { conn.post('invalid-json') }.to raise_error(Faraday::ParsingError) do |ex|
          expect(ex.response).to be_a(Faraday::Response)
          expect(ex.raw_response).to eq(
            status: 200,
            headers: { 'Content-Type' => 'application/json' },
            body: 'not-json'
          )
          expect(ex.current_response).to eq(ex.response)
          expect(ex.current_response.body).to eq('not-json')
          expect(ex.response.env[Faraday::RAW_ADAPTER_RESPONSE_KEY]).to be_nil
        end
      end
    end

    context 'with a nil status response' do
      let(:conn) do
        Faraday.new do |b|
          b.response :raise_error, response_stages: true
          b.adapter :test do |stub|
            stub.get('nil-status') { [nil, { 'X-Reason' => 'nil' }, 'fail'] }
          end
        end
      end

      it 'captures the response body in both stages' do
        expect { conn.get('nil-status') }.to raise_error(Faraday::NilStatusError) do |ex|
          expect(ex.raw_response).to eq(
            status: nil,
            headers: { 'X-Reason' => 'nil' },
            body: 'fail'
          )
          expect(ex.current_response[:body]).to eq('fail')
          expect(ex.response_body).to eq('fail')
        end
      end
    end

    context 'when the response does not raise' do
      let(:conn) do
        Faraday.new do |b|
          b.response :raise_error, response_stages: true, allowed_statuses: [404]
          b.adapter :test do |stub|
            stub.get('ok') { [200, {}, 'fine'] }
            stub.get('allowed') { [404, {}, 'nope'] }
          end
        end
      end

      it 'does not leave the private raw-response marker on the successful env' do
        ok_response = conn.get('ok')
        allowed_response = conn.get('allowed')

        expect(ok_response.env[Faraday::RAW_ADAPTER_RESPONSE_KEY]).to be_nil
        expect(allowed_response.env[Faraday::RAW_ADAPTER_RESPONSE_KEY]).to be_nil
        expect(ok_response.body).to eq('fine')
        expect(allowed_response.body).to eq('nope')
      end
    end
  end

  describe 'default allowed_statuses' do
    it 'cannot be mutated through a middleware instance' do
      middleware = described_class.new(->(env) { env })

      expect { middleware.options[:allowed_statuses] << 404 }.to raise_error(FrozenError)
    end

    it 'still raises errors when the default is used' do
      conn = Faraday.new do |b|
        b.response :raise_error
        b.adapter :test do |stub|
          stub.get('not-found') { [404, {}, 'nope'] }
        end
      end

      expect { conn.get('not-found') }.to raise_error(Faraday::ResourceNotFound)
    end
  end

  describe 'allowing certain status codes' do
    let(:conn) do
      Faraday.new do |b|
        b.response :raise_error, allowed_statuses: [404]
        b.adapter :test do |stub|
          stub.get('bad-request') { [400, { 'X-Reason' => 'because' }, 'keep looking'] }
          stub.get('not-found') { [404, { 'X-Reason' => 'because' }, 'keep looking'] }
        end
      end
    end

    it 'raises an error for status codes that are not explicitly allowed' do
      expect { conn.get('bad-request') }.to raise_error(Faraday::BadRequestError)
    end

    it 'does not raise an error for allowed status codes' do
      expect { conn.get('not-found') }.not_to raise_error
    end
  end

  describe 'errors raised by other middlewares' do
    let(:conn) do
      Faraday.new do |b|
        b.response :json
        b.response :raise_error, **middleware_options
        b.adapter :test do |stub|
          stub.post('invalid-json') { [200, { 'Content-Type' => 'application/json' }, 'not-json'] }
        end
      end
    end
    let(:middleware_options) { {} }

    subject(:perform_request) do
      conn.post('invalid-json') do |req|
        req.headers['Authorization'] = 'Basic 123'
        req.body = 'REQUEST-SECRET'
      end
    end

    it 'includes request info in the ParsingError' do
      expect { perform_request }.to raise_error(Faraday::ParsingError) do |ex|
        expect(ex.response.env.request_headers['Authorization']).to eq('Basic 123')
        expect(ex.response.env.request_body).to eq('REQUEST-SECRET')
      end
    end

    context 'when include_request: false' do
      let(:middleware_options) { { include_request: false } }

      it 'preserves the Response API without including request info in the ParsingError' do
        expect { perform_request }.to raise_error(Faraday::ParsingError) do |ex|
          completed_env = nil

          expect(ex.response).to be_a(Faraday::Response)
          expect(ex.response.on_complete { |env| completed_env = env }).to equal(ex.response)
          expect(completed_env).to be_a(Faraday::Env)
          expect(ex.response.status).to eq(200)
          expect(ex.response.headers['Content-Type']).to include('json')
          expect(ex.response.body).to eq('not-json')
          expect(completed_env.status).to eq(200)
          expect(completed_env.response_headers['Content-Type']).to include('json')
          expect(completed_env.response_body).to eq('not-json')
          expect(completed_env.method).to be_nil
          expect(completed_env.url).to be_nil
          expect(completed_env.request_headers).to be_nil
          expect(completed_env.request_body).to be_nil
          expect(completed_env[:include_request]).to be_nil
          expect(completed_env.response).to be_nil
          expect(ex.response.inspect).not_to include('Basic 123')
          expect(ex.response.inspect).not_to include('REQUEST-SECRET')
          expect(ex.inspect).not_to include('Basic 123')
          expect(ex.inspect).not_to include('REQUEST-SECRET')
        end
      end

      context 'and when the response has no status' do
        subject(:perform_request) do
          conn.post('invalid-json') do |req|
            req.headers['Authorization'] = 'Basic 123'
            req.body = 'REQUEST-SECRET'
          end
        end

        let(:conn) do
          # RaiseError must be registered before Json so that Json raises the
          # ParsingError for the nil-status response.
          Faraday.new do |b|
            b.response :raise_error, include_request: false
            b.response :json
            b.adapter :test do |stub|
              stub.post('invalid-json') { [nil, { 'Content-Type' => 'application/json' }, 'not-json'] }
            end
          end
        end

        it 'does not include request info in the ParsingError either' do
          expect { perform_request }.to raise_error(Faraday::ParsingError) do |ex|
            expect(ex.response).to be_a(Faraday::Response)
            expect(ex.response_body).to be_nil
            expect(ex.response.env.response_body).to be_nil
            expect(ex.response.env.request_body).to be_nil
            expect(ex.response.inspect).not_to include('REQUEST-SECRET')
            expect(ex.inspect).not_to include('REQUEST-SECRET')
          end
        end
      end
    end
  end
end
