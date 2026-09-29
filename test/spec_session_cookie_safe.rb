# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require_relative 'helper'
require 'rack/response'
require 'rack/lint'
require 'rack/mock'

require_relative '../lib/rack/session/cookie'

describe Rack::Session::Cookie::Safe do
  let(:coder) { Rack::Session::Cookie::Safe.new }

  def decode_error
    Rack::Session::Cookie::Safe::DecodeError
  end

  def encode_error
    Rack::Session::Cookie::Safe::EncodeError
  end

  it 'has an explicit version' do
    Rack::Session::Cookie::Safe::VERSION.must_equal 1
  end

  it 'prefixes the payload with a magic header and version' do
    coder.encode({})[0, 4].must_equal 'RSC1'
  end

  it 'round-trips all supported types' do
    object = {
      'string' => "hello \xE2\x98\x83".dup.force_encoding(Encoding::UTF_8),
      'true' => true,
      'false' => false,
      'nil' => nil,
      'integer' => 42,
      'negative' => -17,
      'float' => 3.25,
      'time' => Time.now,
      'array' => [1, 'two', [3.5, nil]],
      'hash' => {'nested' => {'key' => 'value'}},
    }

    coder.decode(coder.encode(object)).must_equal object
  end

  it 'round-trips large integers' do
    [2**64, 2**512, -(2**512), 0].each do |integer|
      coder.decode(coder.encode(integer)).must_equal integer
    end
  end

  it 'round-trips times with their timezone offset and nanoseconds' do
    time = Time.new(2026, 9, 30, 12, 30, Rational(15123456789, 10**9), '+08:00')

    decoded = coder.decode(coder.encode(time))

    decoded.must_equal time
    decoded.utc_offset.must_equal 8 * 3600
    decoded.nsec.must_equal 123456789
  end

  it 'round-trips times in other timezones' do
    ['-05:30', '+14:00', 'UTC'].each do |zone|
      time = Time.new(2026, 1, 1, 0, 0, 0, zone)
      decoded = coder.decode(coder.encode(time))
      decoded.must_equal time
      decoded.utc_offset.must_equal time.utc_offset
    end
  end

  it 'round-trips deeply nested structures' do
    object = {'level' => 0}
    current = object
    50.times do |index|
      current['child'] = {'level' => index + 1, 'siblings' => [index, 'x']}
      current = current['child']
    end

    coder.decode(coder.encode(object)).must_equal object
  end

  it 'only constructs safe types when decoding' do
    object = {
      'string' => 'value',
      'integer' => 1,
      'float' => 1.5,
      'time' => Time.now,
      'array' => [true, false, nil],
      'hash' => {'key' => 'value'},
    }

    seen = []
    collect = lambda do |value|
      seen << value.class
      case value
      when Array then value.each(&collect)
      when Hash then value.each(&collect)
      end
    end
    collect.call(coder.decode(coder.encode(object)))

    (seen - [String, Integer, Float, Time, Array, Hash, TrueClass, FalseClass, NilClass]).must_be :empty?
  end

  it 'never instantiates application classes when decoding' do
    instantiated = false
    explosive = Class.new do
      define_method(:initialize) do |*|
        instantiated = true
        raise 'this class must never be instantiated by the decoder'
      end
    end

    # A payload containing what a less strict format might interpret as a
    # class reference must be rejected outright:
    lambda {
      coder.decode("RSC1o:#{explosive}")
    }.must_raise decode_error

    # Decoding ordinary payloads mentioning the class as data must only
    # ever produce plain strings:
    coder.decode(coder.encode({'class' => explosive.to_s})).must_equal({'class' => explosive.to_s})

    instantiated.must_equal false
  end

  it 'returns nil when decoding nil' do
    coder.decode(nil).must_be_nil
  end

  describe 'encoding errors' do
    it 'rejects non-finite floats' do
      lambda { coder.encode(Float::INFINITY) }.must_raise encode_error
      lambda { coder.encode(-Float::INFINITY) }.must_raise encode_error
      lambda { coder.encode(Float::NAN) }.must_raise encode_error
    end

    it 'rejects unsupported types' do
      lambda { coder.encode(:symbol) }.must_raise encode_error
      lambda { coder.encode(Object.new) }.must_raise encode_error
      lambda { coder.encode(Rational(1, 2)) }.must_raise encode_error
      lambda { coder.encode(/regexp/) }.must_raise encode_error
    end

    it 'rejects hashes with non-string keys' do
      lambda { coder.encode(1 => 'one') }.must_raise encode_error
      lambda { coder.encode(:key => 'value') }.must_raise encode_error
      lambda { coder.encode('outer' => {1 => 'one'}) }.must_raise encode_error
    end

    it 'rejects circular structures deterministically' do
      array = []
      array << array
      lambda { coder.encode(array) }.must_raise encode_error

      hash = {}
      hash['self'] = hash
      lambda { coder.encode(hash) }.must_raise encode_error

      # Indirect cycles are also detected:
      first = []
      second = [first]
      first << second
      lambda { coder.encode(first) }.must_raise encode_error
    end

    it 'rejects structures deeper than the maximum depth' do
      maximum = Rack::Session::Cookie::Safe::MAXIMUM_DEPTH

      value = nil
      maximum.times { value = [value] }
      coder.decode(coder.encode(value)).must_equal value

      (maximum + 1).times { value = [value] }
      lambda { coder.encode(value) }.must_raise encode_error
    end
  end

  describe 'decoding errors' do
    it 'rejects an unknown format version' do
      lambda { coder.decode('RSC2n') }.must_raise decode_error
      lambda { coder.decode('RSC0n') }.must_raise decode_error
      lambda { coder.decode('') }.must_raise decode_error
      lambda { coder.decode('n') }.must_raise decode_error
    end

    it 'rejects unknown type tags' do
      lambda { coder.decode('RSC1x') }.must_raise decode_error
      lambda { coder.decode('RSC1a2:n?') }.must_raise decode_error
    end

    it 'rejects duplicate hash keys' do
      # h2: s1:a n s1:a n -- the key "a" appears twice:
      lambda { coder.decode('RSC1h2:s1:ans1:an') }.must_raise decode_error
    end

    it 'rejects non-string hash keys' do
      lambda { coder.decode('RSC1h1:i1;n') }.must_raise decode_error
    end

    it 'rejects truncated input' do
      lambda { coder.decode('RSC1s5:ab') }.must_raise decode_error
      lambda { coder.decode('RSC1a2:n') }.must_raise decode_error
      lambda { coder.decode('RSC1i12') }.must_raise decode_error
      lambda { coder.decode('RSC1T1;2;') }.must_raise decode_error
    end

    it 'rejects trailing data' do
      lambda { coder.decode('RSC1nn') }.must_raise decode_error
    end

    it 'rejects malformed integers and floats' do
      lambda { coder.decode('RSC1i;') }.must_raise decode_error
      lambda { coder.decode('RSC1i1x;') }.must_raise decode_error
      lambda { coder.decode('RSC1d;') }.must_raise decode_error
      lambda { coder.decode('RSC1dlol;') }.must_raise decode_error
    end

    it 'rejects non-finite floats' do
      lambda { coder.decode('RSC1d1e999;') }.must_raise decode_error
    end

    it 'rejects invalid times' do
      lambda { coder.decode('RSC1T0;1000000000;0;') }.must_raise decode_error
      lambda { coder.decode('RSC1T0;0;86400;') }.must_raise decode_error
    end

    it 'rejects structures deeper than the maximum depth' do
      maximum = Rack::Session::Cookie::Safe::MAXIMUM_DEPTH
      payload = 'RSC1' + ('a1:' * (maximum + 1)) + 'n'
      lambda { coder.decode(payload) }.must_raise decode_error
    end

    it 'rejects impossible element counts' do
      lambda { coder.decode('RSC1a999999999999:n') }.must_raise decode_error
      lambda { coder.decode('RSC1h999999999999:n') }.must_raise decode_error
    end
  end

  describe 'middleware integration' do
    def response_for(app, options, cookie = nil)
      request_options = {}
      request_options['HTTP_COOKIE'] = cookie if cookie

      middleware = Rack::Session::Cookie.new(app, options)
      middleware = Rack::Lint.new(middleware)
      Rack::MockRequest.new(middleware).get('/', request_options)
    end

    it 'round-trips the session through a cookie' do
      incrementor = lambda do |env|
        env['rack.session']['counter'] ||= 0
        env['rack.session']['counter'] += 1
        hash = env['rack.session'].dup
        hash.delete('session_id')
        Rack::Response.new(hash.inspect).to_a
      end

      options = {coder: coder, let_coder_handle_secure_encoding: true}

      response = response_for(incrementor, options)
      response['Set-Cookie'].must_include 'rack.session='
      response.body.must_equal({'counter' => 1}.to_s)

      cookie = response['Set-Cookie'].split(';', 2).first
      response = response_for(incrementor, options, cookie)
      response.body.must_equal({'counter' => 2}.to_s)
    end

    it 'does not write any cookie when encoding fails' do
      unencodable = lambda do |env|
        env['rack.session']['object'] = Object.new
        Rack::Response.new('OK').to_a
      end

      options = {coder: coder, let_coder_handle_secure_encoding: true}

      response = response_for(unencodable, options)
      response['Set-Cookie'].must_be_nil
    end
  end
end
