# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2022-2023, by Samuel Williams.
# Copyright, 2022, by Jeremy Evans.

require_relative 'helper'
require 'rack/response'
require 'rack/lint'
require 'rack/mock'
require 'json'

require_relative '../lib/rack/session/cookie'

describe Rack::Session::Cookie do
  incrementor = lambda do |env|
    env["rack.session"]["counter"] ||= 0
    env["rack.session"]["counter"] += 1
    hash = env["rack.session"].dup
    hash.delete("session_id")
    Rack::Response.new(hash.inspect).to_a
  end

  session_id = lambda do |env|
    Rack::Response.new(env["rack.session"].to_hash.inspect).to_a
  end

  session_option = lambda do |opt|
    lambda do |env|
      Rack::Response.new(env["rack.session.options"][opt].inspect).to_a
    end
  end

  nothing = lambda do |env|
    Rack::Response.new("Nothing").to_a
  end

  renewer = lambda do |env|
    env["rack.session.options"][:renew] = true
    Rack::Response.new("Nothing").to_a
  end

  only_session_id = lambda do |env|
    Rack::Response.new(env["rack.session"]["session_id"].to_s).to_a
  end

  bigcookie = lambda do |env|
    env["rack.session"]["cookie"] = "big" * 3000
    Rack::Response.new(env["rack.session"].inspect).to_a
  end

  sizer = lambda do |env|
    size = Rack::Request.new(env).params["size"].to_i
    env["rack.session"]["payload"] = "x" * size
    Rack::Response.new("ok").to_a
  end

  payload_size = lambda do |env|
    Rack::Response.new(env["rack.session"]["payload"].to_s.bytesize.to_s).to_a
  end

  destroy_session = lambda do |env|
    env["rack.session"].destroy
    Rack::Response.new("Nothing").to_a
  end

  def response_for(options = {})
    request_options = options.fetch(:request, {})
    cookie = if options[:cookie].is_a?(Rack::Response)
      options[:cookie]["Set-Cookie"]
    else
      options[:cookie]
    end
    request_options["HTTP_COOKIE"] = cookie || ""

    app_with_cookie = Rack::Session::Cookie.new(*options[:app])
    app_with_cookie = Rack::Lint.new(app_with_cookie)
    Rack::MockRequest.new(app_with_cookie).get("/", request_options)
  end

  # Simulates a browser cookie jar: applies the Set-Cookie headers of the
  # response to the jar, honoring cookie deletions.
  def extract_cookies(response, jar = {})
    Array(response["Set-Cookie"]).each do |header|
      pair, attributes = header.split(';', 2)
      name, value = pair.split('=', 2)

      if attributes =~ /max-age=0|expires=Thu, 01 Jan 1970/
        jar.delete(name)
      else
        jar[name] = value
      end
    end

    jar
  end

  def cookie_header_for(jar)
    jar.map { |name, value| "#{name}=#{value}" }.join('; ')
  end

  def random_encryptor_secret
    SecureRandom.random_bytes(64)
  end

  before do
    # Random key, as a hex string
    @secret = random_encryptor_secret

    @warnings = warnings = []
    Rack::Session::Cookie.class_eval do
      define_method(:warn) { |m| warnings << m }
    end
  end

  after do
    Rack::Session::Cookie.class_eval { remove_method :warn }
  end

  describe 'Base64' do
    it 'uses base64 to encode' do
      coder = Rack::Session::Cookie::Base64.new
      str   = 'fuuuuu'
      coder.encode(str).must_equal [str].pack('m0')
    end

    it 'uses base64 to decode' do
      coder = Rack::Session::Cookie::Base64.new
      str   = ['fuuuuu'].pack('m0')
      coder.decode(str).must_equal str.unpack('m0').first
    end

    it 'handles non-strict base64 encoding' do
      coder = Rack::Session::Cookie::Base64.new
      str   = ['A' * 256].pack('m')
      coder.decode(str).must_equal 'A' * 256
    end

    describe 'Marshal' do
      it 'marshals and base64 encodes' do
        coder = Rack::Session::Cookie::Base64::Marshal.new
        str   = 'fuuuuu'
        coder.encode(str).must_equal [::Marshal.dump(str)].pack('m0')
      end

      it 'marshals and base64 decodes' do
        coder = Rack::Session::Cookie::Base64::Marshal.new
        str   = [::Marshal.dump('fuuuuu')].pack('m0')
        coder.decode(str).must_equal ::Marshal.load(str.unpack('m0').first)
      end

      it 'rescues failures on decode' do
        coder = Rack::Session::Cookie::Base64::Marshal.new
        coder.decode('lulz').must_be_nil
      end
    end

    describe 'JSON' do
      it 'JSON and base64 encodes' do
        coder = Rack::Session::Cookie::Base64::JSON.new
        obj   = %w[fuuuuu]
        coder.encode(obj).must_equal [::JSON.dump(obj)].pack('m0')
      end

      it 'JSON and base64 decodes' do
        coder = Rack::Session::Cookie::Base64::JSON.new
        str   = [::JSON.dump(%w[fuuuuu])].pack('m0')
        coder.decode(str).must_equal ::JSON.parse(str.unpack('m0').first)
      end

      it 'rescues failures on decode' do
        coder = Rack::Session::Cookie::Base64::JSON.new
        coder.decode('lulz').must_be_nil
      end
    end

    describe 'ZipJSON' do
      it 'jsons, deflates, and base64 encodes' do
        coder = Rack::Session::Cookie::Base64::ZipJSON.new
        obj   = %w[fuuuuu]
        json = JSON.dump(obj)
        coder.encode(obj).must_equal [Zlib::Deflate.deflate(json)].pack('m0')
      end

      it 'base64 decodes, inflates, and decodes json' do
        coder = Rack::Session::Cookie::Base64::ZipJSON.new
        obj   = %w[fuuuuu]
        json  = JSON.dump(obj)
        b64   = [Zlib::Deflate.deflate(json)].pack('m0')
        coder.decode(b64).must_equal obj
      end

      it 'rescues failures on decode' do
        coder = Rack::Session::Cookie::Base64::ZipJSON.new
        coder.decode('lulz').must_be_nil
      end
    end
  end

  it "warns if no secret is given" do
    Rack::Session::Cookie.new(incrementor)
    @warnings.first.must_match(/no secret/i)
    @warnings.clear
    Rack::Session::Cookie.new(incrementor, secrets: @secret)
    @warnings.must_be :empty?
  end

  it 'abort if secret is too short' do
    lambda {
      Rack::Session::Cookie.new(incrementor, secrets: @secret[0, 16])
    }.must_raise ArgumentError
  end

  it "doesn't warn if coder is configured to handle encoding" do
    Rack::Session::Cookie.new(
      incrementor,
      coder: Object.new,
      let_coder_handle_secure_encoding: true)
    @warnings.must_be :empty?
  end

  it "still warns if coder is not set" do
    Rack::Session::Cookie.new(
      incrementor,
      let_coder_handle_secure_encoding: true)
    @warnings.first.must_match(/no secret/i)
  end

  it 'uses a coder' do
    identity = Class.new {
      attr_reader :calls

      def initialize
        @calls = []
      end

      def encode(hash); @calls << :encode; hash.to_json; end
      def decode(str); @calls << :decode; JSON.parse(str); end
    }.new
    response = response_for(app: [incrementor, { coder: identity }])

    response["Set-Cookie"].must_include "rack.session="
    response.body.must_equal ({"counter"=>1}.to_s)
    identity.calls.must_equal [:encode]

    response_for(app: [incrementor, { coder: identity }], cookie: response["Set-Cookie"].split(';', 2).first)
    identity.calls.must_equal [:encode, :decode, :encode]
  end

  it "creates a new cookie" do
    response = response_for(app: incrementor)
    response["Set-Cookie"].must_include "rack.session="
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it "passes through same_site option to session cookie" do
    response = response_for(app: [incrementor, same_site: :none])
    response["Set-Cookie"].must_match /SameSite=None/i
  end

  it "allows using a lambda to specify same_site option, because some browsers require different settings" do
    # Details of why this might need to be set dynamically:
    # https://www.chromium.org/updates/same-site/incompatible-clients
    # https://gist.github.com/bnorton/7dee72023787f367c48b3f5c2d71540f

    response = response_for(app: [incrementor, same_site: lambda { |req, res| :none }])
    response["Set-Cookie"].must_match /SameSite=None/i

    response = response_for(app: [incrementor, same_site: lambda { |req, res| :lax }])
    response["Set-Cookie"].must_match /SameSite=Lax/i
  end

  it "loads from a cookie" do
    response = response_for(app: incrementor)

    response = response_for(app: incrementor, cookie: response)
    response.body.must_equal ({"counter"=>2}.to_s)

    response = response_for(app: incrementor, cookie: response)
    response.body.must_equal ({"counter"=>3}.to_s)
  end

  it "renew session id" do
    response = response_for(app: incrementor)
    cookie   = response['Set-Cookie']
    response = response_for(app: only_session_id, cookie: cookie)
    cookie   = response['Set-Cookie'] if response['Set-Cookie']

    response.body.wont_equal ""
    old_session_id = response.body

    response = response_for(app: renewer, cookie: cookie)
    cookie   = response['Set-Cookie'] if response['Set-Cookie']
    response = response_for(app: only_session_id, cookie: cookie)

    response.body.wont_equal ""
    response.body.wont_equal old_session_id
  end

  it "destroys session" do
    response = response_for(app: incrementor)
    response = response_for(app: only_session_id, cookie: response)

    response.body.wont_equal ""
    old_session_id = response.body

    response = response_for(app: destroy_session, cookie: response)
    response = response_for(app: only_session_id, cookie: response)

    response.body.wont_equal ""
    response.body.wont_equal old_session_id
  end

  it "survives broken cookies" do
    response = response_for(
      app: incrementor,
      cookie: "rack.session=blarghfasel"
    )
    response.body.must_equal ({"counter"=>1}.to_s)

    response = response_for(
      app: [incrementor, { secrets: @secret }],
      cookie: "rack.session="
    )
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it "barks on too big cookies" do
    lambda{
      response_for(app: bigcookie, request: { fatal: true })
    }.must_raise Rack::MockRequest::FatalWarning
  end

  it "loads from a cookie with encryption" do
    app = [incrementor, { secrets: @secret }]

    response = response_for(app: app)
    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>2}.to_s)

    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>3}.to_s)

    app = [incrementor, { secrets: random_encryptor_secret }]

    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it "loads from a cookie with accept-only integrity hash for graceful key rotation" do
    response = response_for(app: [incrementor, { secrets: @secret }])

    new_secret = random_encryptor_secret

    app = [incrementor, { secrets: [new_secret, @secret] }]
    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>2}.to_s)

    newer_secret = random_encryptor_secret

    app = [incrementor, { secrets: [newer_secret, new_secret] }]
    response = response_for(app: app, cookie: response)

    response.body.must_equal ({"counter"=>3}.to_s)
  end

  it 'loads from a legacy hmac cookie' do
    legacy_session = Rack::Session::Cookie::Base64::Marshal.new.encode({ 'counter' => 1, 'session_id' => 'abcdef' })
    legacy_secret  = 'test legacy secret'
    legacy_digest  = OpenSSL::HMAC.hexdigest(OpenSSL::Digest::SHA1.new, legacy_secret, legacy_session)

    legacy_cookie = "rack.session=#{legacy_session}--#{legacy_digest}; path=/; HttpOnly"

    app = [incrementor, { secrets: @secret, legacy_hmac_secret: legacy_secret }]
    response = response_for(app: app, cookie: legacy_cookie)
    response.body.must_equal ({"counter"=>2}.to_s)
  end

  it "ignores tampered session cookies" do
    app = [incrementor, { secrets: @secret }]

    response = response_for(app: app)
    response.body.must_equal ({"counter"=>1}.to_s)

    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>2}.to_s)

    encoded_cookie = response["Set-Cookie"].split('=', 2).last.split(';').first
    decoded_cookie = Base64.urlsafe_decode64(Rack::Utils.unescape(encoded_cookie))

    tampered_cookie = "rack.session=#{Base64.urlsafe_encode64(decoded_cookie.tap { |m|
      m[m.size - 1] = (m[m.size - 1].unpack('C')[0] ^ 1).chr
    })}"

    response = response_for(app: app, cookie: tampered_cookie)
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it 'rejects a forged plain Base64::Marshal cookie when secrets: is configured' do
    # Construct a forged cookie without knowing the secret: plain Base64-encoded
    # Marshal payload, identical to what an attacker would send.
    forged_payload = { 'session_id' => 'attacker-fixed', 'counter' => 999 }
    forged_cookie = "rack.session=#{Base64.strict_encode64(Marshal.dump(forged_payload))}"

    app = [incrementor, { secrets: @secret }]

    # The forged cookie must be rejected; session starts fresh (counter = 1).
    response = response_for(app: app, cookie: forged_cookie)
    response.body.must_equal ({"counter" => 1}.to_s)
  end

  it 'rejects a forged plain Base64::Marshal cookie when secrets: and serialize_json: true are configured' do
    # serialize_json: true only affects the encryptor serializer; the coder
    # fallback must also be suppressed when encryptors are configured.
    forged_payload = { 'session_id' => 'attacker-fixed', 'counter' => 999 }
    forged_cookie = "rack.session=#{Base64.strict_encode64(Marshal.dump(forged_payload))}"

    app = [incrementor, { secrets: @secret, serialize_json: true }]

    response = response_for(app: app, cookie: forged_cookie)
    response.body.must_equal ({"counter" => 1}.to_s)
  end

  it 'rejects session cookie with different purpose' do
    app = [incrementor, { secrets: @secrets }]
    other_app = [incrementor, { secrets: @secrets, key: 'other' }]

    response = response_for(app: app)
    response.body.must_equal ({"counter"=>1}.to_s)

    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>2}.to_s)

    response = response_for(app: other_app, cookie: response)
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it 'adds to RACK_ERRORS on encryptor errors' do
    echo_rack_errors = lambda do |env|
      env["rack.session"]["counter"] ||= 0
      env["rack.session"]["counter"] += 1
      Rack::Response.new(env[Rack::RACK_ERRORS].flush.tap(&:rewind).read).to_a
    end

    app = [incrementor, { secrets: @secret }]
    err_app = [echo_rack_errors, { secrets: @secret }]

    response = response_for(app: app)
    response.body.must_equal ({"counter"=>1}.to_s)

    encoded_cookie = response["Set-Cookie"].split('=', 2).last.split(';').first
    decoded_cookie = Base64.urlsafe_decode64(Rack::Utils.unescape(encoded_cookie))

    tampered_cookie = "rack.session=#{Base64.urlsafe_encode64(decoded_cookie.tap { |m|
      m[m.size - 1] = "\0"
    })}"

    response = response_for(app: err_app, cookie: tampered_cookie)
    response.body.must_equal "Session cookie encryptor error: invalid message\n"
  end

  it 'ignores tampered with legacy hmac cookie' do
    legacy_session = Rack::Session::Cookie::Base64::Marshal.new.encode({ 'counter' => 1, 'session_id' => 'abcdef' })
    legacy_secret  = 'test legacy secret'
    legacy_digest  = OpenSSL::HMAC.hexdigest(OpenSSL::Digest::SHA1.new, legacy_secret, legacy_session).reverse

    legacy_cookie = "rack.session=#{legacy_session}--#{legacy_digest}; path=/; HttpOnly"

    app = [incrementor, { secret: @secret, legacy_hmac_secret: legacy_secret }]
    response = response_for(app: app, cookie: legacy_cookie)
    response.body.must_equal ({"counter"=>1}.to_s)
  end

  it "supports custom digest instance for legacy hmac cookie" do
    legacy_hmac    = 'SHA256'
    legacy_session = Rack::Session::Cookie::Base64::Marshal.new.encode({ 'counter' => 1, 'session_id' => 'abcdef' })
    legacy_secret  = 'test legacy secret'
    legacy_digest  = OpenSSL::HMAC.hexdigest(legacy_hmac, legacy_secret, legacy_session)
    legacy_cookie = "rack.session=#{legacy_session}--#{legacy_digest}; path=/; HttpOnly"

    app = [incrementor, {
      secrets: @secret, legacy_hmac_secret: legacy_secret, legacy_hmac: legacy_hmac
    }]

    response = response_for(app: app, cookie: legacy_cookie)
    response.body.must_equal ({"counter"=>2}.to_s)

    response = response_for(app: app, cookie: response)
    response.body.must_equal ({"counter"=>3}.to_s)
  end

  it "can handle Rack::Lint middleware" do
    response = response_for(app: incrementor)

    lint = Rack::Lint.new(session_id)
    response = response_for(app: lint, cookie: response)
    response.body.wont_be :nil?
  end

  it "can handle middleware that inspects the env" do
    class TestEnvInspector
      def initialize(app)
        @app = app
      end
      def call(env)
        env.inspect
        @app.call(env)
      end
    end

    response = response_for(app: incrementor)

    inspector = TestEnvInspector.new(session_id)
    response = response_for(app: inspector, cookie: response)
    response.body.wont_be :nil?
  end

  it "returns the session id in the session hash" do
    response = response_for(app: incrementor)
    response.body.must_equal ({"counter"=>1}.to_s)

    response = response_for(app: session_id, cookie: response)
    response.body.must_match(/"session_id"\s*=>/)
    response.body.must_match(/"counter"\s*=>\s*1/)
  end

  it "does not return a cookie if set to secure but not using ssl" do
    app = [incrementor, { secure: true }]

    response = response_for(app: app)
    response["Set-Cookie"].must_be_nil

    response = response_for(app: app, request: { "HTTPS" => "on" })
    response["Set-Cookie"].wont_be :nil?
    response["Set-Cookie"].must_match(/secure/)
  end

  it "does not return a cookie if cookie was not read/written" do
    response = response_for(app: nothing)
    response["Set-Cookie"].must_be_nil
  end

  it "does not return a cookie if cookie was not written (only read)" do
    response = response_for(app: session_id)
    response["Set-Cookie"].must_be_nil
  end

  it "returns even if not read/written if :expire_after is set" do
    app = [nothing, { expire_after: 3600 }]
    request = { "rack.session" => { "not" => "empty" } }
    response = response_for(app: app, request: request)
    response["Set-Cookie"].wont_be :nil?
  end

  it "returns no cookie if no data was written and no session was created previously, even if :expire_after is set" do
    app = [nothing, { expire_after: 3600 }]
    response = response_for(app: app)
    response["Set-Cookie"].must_be_nil
  end

  it "exposes :secrets in env['rack.session.option']" do
    response = response_for(app: [session_option[:secrets], { secrets: @secret }])
    response.body.must_equal @secret.inspect
  end

  it "exposes :coder in env['rack.session.option']" do
    response = response_for(app: session_option[:coder])
    response.body.must_match(/Base64::Marshal/)
  end

  it 'exposes correct :coder when a secrets is used' do
    response = response_for(app: session_option[:coder], secrets: @secret)
    response.body.must_match(/Marshal/)
  end

  it "allows passing in a hash with session data from middleware in front" do
    request = { 'rack.session' => { foo: 'bar' } }
    response = response_for(app: session_id, request: request)
    response.body.must_match(/foo/)
  end

  it "allows modifying session data with session data from middleware in front" do
    request = { 'rack.session' => { foo: 'bar' } }
    response = response_for(app: incrementor, request: request)
    response.body.must_match(/counter/)
    response.body.must_match(/foo/)
  end

  it "allows more than one '--' in the cookie when calculating legacy digests" do
    @counter = 0
    app = lambda do |env|
      env["rack.session"]["message"] ||= ""
      env["rack.session"]["message"] += "#{(@counter += 1).to_s}--"
      hash = env["rack.session"].dup
      hash.delete("session_id")
      Rack::Response.new(hash["message"]).to_a
    end

    # another example of an unsafe coder is Base64.urlsafe_encode64
    unsafe_coder = Class.new {
      def encode(hash); hash.inspect end
      def decode(str); eval(str) if str; end
    }.new

    legacy_session = unsafe_coder.encode('message' => "#{@counter += 1}--#{@counter += 1}--", 'session_id' => 'abcdef')
    legacy_secret  = 'test legacy secret'
    legacy_digest  = OpenSSL::HMAC.hexdigest(OpenSSL::Digest::SHA1.new, legacy_secret, legacy_session)
    legacy_cookie = "rack.session=#{Rack::Utils.escape legacy_session}--#{legacy_digest}; path=/; HttpOnly"

    _app = [ app, {
      secrets: @secret,
      legacy_hmac_secret: legacy_secret,
      legacy_hmac_coder: unsafe_coder
    }]

    response = response_for(app: _app, cookie: legacy_cookie)
    response.body.must_equal "1--2--3--"
  end

  it 'allows for non-strict encoded cookie' do
    long_session_app = lambda do |env|
      env['rack.session']['value'] = 'A' * 256
      env['rack.session']['counter'] = 1
      hash = env["rack.session"].dup
      hash.delete("session_id")
      Rack::Response.new(hash.inspect).to_a
    end

    non_strict_coder = Class.new {
      def encode(str)
        [Marshal.dump(str)].pack('m')
      end

      def decode(str)
        return unless str

        Marshal.load(str.unpack('m').first)
      end
    }.new

    non_strict_response = response_for(app: [
      long_session_app, { coder: non_strict_coder }
    ])

    response = response_for(app: [
      incrementor
    ], cookie: non_strict_response)

    response.body.must_match /"value"\s*=>\s*"#{'A' * 256}"/
    response.body.must_match /"counter"\s*=>\s*2/
    response.body.must_match(/\A{[^}]+}\z/)
  end

  describe 'chunked cookies' do
    chunk_name = /\Arack\.session\.[0-9a-f]{16}\.\d+\z/
    chunk_header = /\Arack\.session\.[0-9a-f]{16}\.\d+=/

    it 'requires secrets when chunking is enabled' do
      lambda {
        Rack::Session::Cookie.new(incrementor, chunked: true)
      }.must_raise ArgumentError
    end

    it 'writes small sessions as a single cookie when chunking is enabled' do
      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }])
      response["Set-Cookie"].must_include "rack.session="
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'reads plain single cookies when chunking is enabled' do
      response = response_for(app: [incrementor, { secrets: @secret }])
      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }], cookie: response)
      response.body.must_equal ({"counter"=>2}.to_s)
    end

    it 'splits oversized sessions into a manifest and generation-tagged chunks' do
      app = [sizer, { secrets: @secret, chunked: true }]
      response = response_for(app: app, request: { "QUERY_STRING" => "size=9000" })

      cookies = Array(response["Set-Cookie"])
      cookies.size.must_be :>, 2
      cookies.first.must_match(/\Arack\.session=/)
      cookies[1..-1].each { |header| header.must_match(chunk_header) }

      # The chunks reassemble into the original session.
      jar = extract_cookies(response)
      response = response_for(app: [payload_size, { secrets: @secret, chunked: true }], cookie: cookie_header_for(jar))
      response.body.must_equal "9000"
    end

    it 'handles consecutive size changes, deleting stale tail chunks' do
      app = [sizer, { secrets: @secret, chunked: true }]

      # Start small: a single plain cookie.
      response = response_for(app: app, request: { "QUERY_STRING" => "size=10" })
      jar = extract_cookies(response)
      jar.keys.must_equal ["rack.session"]

      # Grow beyond a single cookie: manifest plus chunk cookies.
      response = response_for(app: app, cookie: cookie_header_for(jar), request: { "QUERY_STRING" => "size=9000" })
      jar = extract_cookies(response, jar)
      chunk_count = jar.keys.grep(chunk_name).size
      chunk_count.must_be :>=, 2

      # Shrink within chunking: fewer chunks, stale tail chunks deleted.
      response = response_for(app: app, cookie: cookie_header_for(jar), request: { "QUERY_STRING" => "size=4500" })
      deletions = Array(response["Set-Cookie"]).grep(/max-age=0/)
      jar = extract_cookies(response, jar)
      new_chunk_count = jar.keys.grep(chunk_name).size
      new_chunk_count.must_be :<, chunk_count
      deletions.size.must_equal chunk_count - new_chunk_count

      # Shrink back to a single cookie: all remaining chunks deleted.
      response = response_for(app: app, cookie: cookie_header_for(jar), request: { "QUERY_STRING" => "size=10" })
      deletions = Array(response["Set-Cookie"]).grep(/max-age=0/)
      deletions.size.must_equal new_chunk_count
      jar = extract_cookies(response, jar)
      jar.keys.must_equal ["rack.session"]

      # The shrunken session still reads back correctly.
      response = response_for(app: [payload_size, { secrets: @secret, chunked: true }], cookie: cookie_header_for(jar))
      response.body.must_equal "10"
    end

    it 'rejects the session when the browser returns only some chunks' do
      app = [sizer, { secrets: @secret, chunked: true }]
      response = response_for(app: app, request: { "QUERY_STRING" => "size=9000" })
      jar = extract_cookies(response)

      # The browser "loses" one of the chunk cookies.
      jar.delete(jar.keys.grep(chunk_name).first)

      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }], cookie: cookie_header_for(jar))
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'rejects the session when a chunk cookie is sent twice' do
      app = [sizer, { secrets: @secret, chunked: true }]
      response = response_for(app: app, request: { "QUERY_STRING" => "size=9000" })
      jar = extract_cookies(response)

      chunk_pair = cookie_header_for(jar).split('; ').find { |pair| pair =~ chunk_header }
      cookie = "#{cookie_header_for(jar)}; #{chunk_pair}"

      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }], cookie: cookie)
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'rejects the session when chunks from different generations are mixed' do
      app = [sizer, { secrets: @secret, chunked: true }]

      # Two independent chunked sessions use different generations.
      jar_a = extract_cookies(response_for(app: app, request: { "QUERY_STRING" => "size=9000" }))
      jar_b = extract_cookies(response_for(app: app, request: { "QUERY_STRING" => "size=9000" }))

      # The browser sends one session's manifest with chunks from both.
      jar = jar_a.merge(jar_b)

      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }], cookie: cookie_header_for(jar))
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'rejects the session when a chunk is tampered with' do
      app = [sizer, { secrets: @secret, chunked: true }]
      response = response_for(app: app, request: { "QUERY_STRING" => "size=9000" })
      jar = extract_cookies(response)

      name = jar.keys.grep(chunk_name).first
      jar[name] = jar[name].sub(/[A-Za-z0-9]/) { |char| char == "A" ? "B" : "A" }

      response = response_for(app: [incrementor, { secrets: @secret, chunked: true }], cookie: cookie_header_for(jar))
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'rejects manifests that exceed the chunk limit' do
      manifest = Rack::Session::Encryptor.new(@secret).encrypt(
        "rack.session.chunks" => {
          "generation" => "0" * 16,
          "count" => 5,
          "digest" => Digest::SHA256.hexdigest("chunks")
        }
      )

      response = response_for(
        app: [incrementor, { secrets: @secret, chunked: true }],
        cookie: "rack.session=#{Rack::Utils.escape(manifest)}"
      )
      response.body.must_equal ({"counter"=>1}.to_s)
    end

    it 'refuses to write sessions that exceed the chunk limit' do
      huge = lambda do |env|
        env["rack.session"]["payload"] = "x" * 100_000
        Rack::Response.new("ok").to_a
      end

      lambda {
        response_for(app: [huge, { secrets: @secret, chunked: true }], request: { fatal: true })
      }.must_raise Rack::MockRequest::FatalWarning
    end

    it 'accounts all Set-Cookie headers against the header budget' do
      # The manifest and chunk cookies together exceed the budget: the
      # session is dropped instead of bypassing the limit via chunking.
      app = [sizer, { secrets: @secret, chunked: true, header_budget: 4096 }]
      response = response_for(app: app, request: { "QUERY_STRING" => "size=9000" })
      response["Set-Cookie"].must_be_nil

      # A small session fits within the same budget.
      response = response_for(app: app, request: { "QUERY_STRING" => "size=10" })
      response["Set-Cookie"].wont_be_nil
    end
  end
end
