require_relative 'test_helper'

# End-to-end exercise of 206 fragment accumulation through the middleware.
describe 'fragment storage integration' do
  FULL_BODY = ('a'..'j').to_a.join # "abcdefghij", 10 bytes

  # Backend which answers Range requests with proper 206 responses and plain
  # GET requests with the whole resource. @handler may override behavior for
  # failure scenarios.
  class RangeBackend
    attr_accessor :plain_gets, :range_gets, :handler

    def initialize(body, etag)
      @body = body
      @etag = etag
      @plain_gets = 0
      @range_gets = 0
      @handler = nil
    end

    def call(env)
      request = Rack::Request.new(env)
      if @handler
        result = @handler.call(env, request)
        return result if result
      end

      headers = {
        'etag' => @etag,
        'cache-control' => 'max-age=300',
        'content-type' => 'application/octet-stream',
        'accept-ranges' => 'bytes'
      }

      if (range = request.env['HTTP_RANGE'])
        @range_gets += 1
        if range =~ /\Abytes=(\d*)-(\d*)\z/
          start_str, finish_str = $1, $2
          start = start_str.empty? ? nil : start_str.to_i
          finish = finish_str.empty? ? @body.bytesize - 1 : finish_str.to_i
          start ||= @body.bytesize - finish
          finish = @body.bytesize - 1 if finish >= @body.bytesize
          chunk = @body.byteslice(start, finish - start + 1)
          headers['content-range'] =
            "bytes #{start}-#{finish}/#{@body.bytesize}"
          return [206, headers, [chunk]]
        end
        return [416, headers.merge('content-range' => "bytes */#{@body.bytesize}"), []]
      end

      @plain_gets += 1
      [200, headers, [@body]]
    end
  end

  def build_cache(backend, config = {})
    @storage = Rack::Cache::Storage.new
    defaults = { 'rack-cache.store_fragments' => true }
    context = Rack::Cache::Context.new(backend) do |c|
      c.set defaults.merge(config)
    end
    context
  end

  def invoke(context, method, uri, env = {})
    opts = {
      'rack.run_once' => true,
      'rack.multithread' => false,
      'rack.errors' => StringIO.new,
      'rack-cache.storage' => @storage
    }.merge(env)
    Rack::MockRequest.new(context).request(method, uri, opts)
  end

  def range_request(context, range, env = {})
    invoke(context, 'GET', '/download', env.merge('HTTP_RANGE' => range))
  end

  def plain_get(context, env = {})
    invoke(context, 'GET', '/download', env)
  end

  def metastore(context)
    context.metastore
  end

  def cached_entry(context, env = {})
    request = Rack::Cache::Request.new(
      Rack::MockRequest.env_for('/download', env))
    metastore(context).lookup(request, context.entitystore)
  end

  before do
    @backend = RangeBackend.new(FULL_BODY, '"v1"')
    @cache = build_cache(@backend)
  end

  it 'forwards 206 responses to the client while filling the cache' do
    response = range_request(@cache, 'bytes=0-3')
    response.status.must_equal 206
    response.body.must_equal 'abcd'
    response.headers['x-rack-cache'].
      must_include 'fragment_stored'
    response.headers['x-rack-cache'].
      must_include 'fragment_incomplete'
  end

  it 'does not publish a complete entry until every byte is covered' do
    range_request(@cache, 'bytes=0-3')
    assert_nil cached_entry(@cache), 'no entry while gaps remain'

    range_request(@cache, 'bytes=7-9')
    assert_nil cached_entry(@cache), 'still gaps in the middle'

    # an ordinary GET must not be satisfied by the partial assembly; it goes
    # to the origin and is stored as a normal full response
    response = plain_get(@cache)
    response.status.must_equal 200
    response.body.must_equal FULL_BODY
    response.headers['x-rack-cache'].must_include 'miss'
    @backend.plain_gets.must_equal 1
    refute_nil cached_entry(@cache), 'full response cached by the normal path'

    # the next plain GET is served without contacting the origin again
    response = plain_get(@cache)
    response.headers['x-rack-cache'].must_include 'fresh'
    @backend.plain_gets.must_equal 1
  end

  it 'serves plain GETs from the origin until complete, then from cache' do
    range_request(@cache, 'bytes=0-3')
    range_request(@cache, 'bytes=7-9')

    response = plain_get(@cache)
    response.status.must_equal 200
    response.body.must_equal FULL_BODY
    response.headers['x-rack-cache'].must_include 'miss'
    @backend.plain_gets.must_equal 1

    # once the origin-filled response is stored, later GETs hit the cache
    response = plain_get(@cache)
    response.headers['x-rack-cache'].must_include 'fresh'
    response.body.must_equal FULL_BODY
    @backend.plain_gets.must_equal 1
  end

  it 'serves the assembled body to a plain GET at the promotion moment' do
    # arrives out of order; after these, bytes 0-8 are covered and byte 9 gaps
    [[0, 2], [6, 8], [3, 5]].each do |s, f|
      range_request(@cache, "bytes=#{s}-#{f}")
    end
    assert_nil cached_entry(@cache)

    response = range_request(@cache, 'bytes=9-9')
    response.status.must_equal 206
    response.body.must_equal 'j'
    response.headers['x-rack-cache'].must_include 'fragment_complete'

    entry = cached_entry(@cache)
    refute_nil entry
    body = +''
    entry.body.each { |part| body << part }
    body.must_equal FULL_BODY
    entry.status.must_equal 200
    entry.headers['content-length'].must_equal '10'
    entry.headers['etag'].must_equal '"v1"'

    # a later plain GET is satisfied from cache without contacting origin
    response = plain_get(@cache)
    response.headers['x-rack-cache'].must_include 'fresh'
    response.body.must_equal FULL_BODY
    @backend.plain_gets.must_equal 0
  end

  it 'accepts duplicate and agreeing overlapping fragments' do
    range_request(@cache, 'bytes=0-4')
    range_request(@cache, 'bytes=0-4')        # duplicate
    range_request(@cache, 'bytes=3-6')        # agrees on bytes 3-4
    assert_nil cached_entry(@cache)

    response = range_request(@cache, 'bytes=7-9')
    response.headers['x-rack-cache'].must_include 'fragment_complete'
    plain = plain_get(@cache)
    plain.body.must_equal FULL_BODY
    @backend.plain_gets.must_equal 0
  end

  it 'rejects disagreeing overlaps and never publishes an entry' do
    @backend.handler = proc do |env, request|
      if env['HTTP_RANGE'] == 'bytes=3-5'
        [206, {
          'etag' => '"v1"',
          'cache-control' => 'max-age=300',
          'content-type' => 'application/octet-stream',
          'content-range' => 'bytes 3-5/10'
        }, ['ZZZ']]
      end
    end

    range_request(@cache, 'bytes=0-4')
    response = range_request(@cache, 'bytes=3-5')

    # the conflicting 206 is still delivered to the client
    response.status.must_equal 206
    response.body.must_equal 'ZZZ'
    response.headers['x-rack-cache'].
      must_include 'fragment_rejected:overlap_conflict'

    assert_nil cached_entry(@cache)
    # the good fragments are retained for later completion
    range_request(@cache, 'bytes=3-9')
    response = plain_get(@cache)
    response.body.must_equal FULL_BODY
  end

  it 'rejects responses that end early and keeps prior fragments intact' do
    @backend.handler = proc do |_env, request|
      if request.env['HTTP_RANGE'] == 'bytes=4-7'
        [206, {
          'etag' => '"v1"',
          'cache-control' => 'max-age=300',
          'content-type' => 'application/octet-stream',
          'content-range' => 'bytes 4-7/10'
        }, ['ef']] # 4 bytes advertised, 2 delivered
      end
    end

    range_request(@cache, 'bytes=0-3')
    response = range_request(@cache, 'bytes=4-7')
    response.status.must_equal 206
    response.body.must_equal 'ef'
    response.headers['x-rack-cache'].
      must_include 'fragment_rejected:length_mismatch'
    assert_nil cached_entry(@cache)
  end

  it 'rejects contradictory total lengths for the same representation' do
    @backend.handler = proc do |_env, request|
      if request.env['HTTP_RANGE'] == 'bytes=4-7'
        [206, {
          'etag' => '"v1"',
          'cache-control' => 'max-age=300',
          'content-type' => 'application/octet-stream',
          'content-range' => 'bytes 4-7/11'
        }, ['efgh']]
      end
    end

    range_request(@cache, 'bytes=0-3')
    response = range_request(@cache, 'bytes=4-7')
    response.headers['x-rack-cache'].
      must_include 'fragment_rejected:total_length_mismatch'
    assert_nil cached_entry(@cache)
  end

  it 'isolates representation versions after the validator changes' do
    range_request(@cache, 'bytes=0-4') # v1 first half

    # origin regenerates: new strong ETag, new bytes
    v2_backend = RangeBackend.new('ABCDEFGHIJ', '"v2"')
    v2_cache = build_cache(v2_backend)
    # share the same fragment store so the version switch is visible
    v2_cache.instance_variable_set(:@fragment_store,
                                   @cache.fragment_store)

    response = range_request(v2_cache, 'bytes=0-4')
    response.headers['x-rack-cache'].must_include 'fragment_incomplete'
    assert_nil cached_entry(v2_cache)

    # old v1 fragments cannot complete the v2 representation even though they
    # cover the missing tail of the URI
    response = range_request(v2_cache, 'bytes=5-9')
    response.headers['x-rack-cache'].must_include 'fragment_complete'

    entry = cached_entry(v2_cache)
    refute_nil entry
    body = +''
    entry.body.each { |part| body << part }
    body.must_equal 'ABCDEFGHIJ'
    entry.headers['etag'].must_equal '"v2"'
  end

  it 'isolates Vary-negotiated variants' do
    backend = RangeBackend.new(FULL_BODY, '"v1"')
    backend.handler = proc do |env, _request|
      if env['HTTP_RANGE'] && env['HTTP_ACCEPT_ENCODING'] == 'gzip'
        [206, {
          'etag' => '"gzip"',
          'cache-control' => 'max-age=300',
          'vary' => 'Accept-Encoding',
          'content-type' => 'application/octet-stream',
          'content-range' => 'bytes 0-9/10'
        }, ['0123456789']]
      end
    end
    cache = build_cache(backend)

    # a complete but gzip-varying response must not be an identity entry
    range_request(cache, 'bytes=0-9', 'HTTP_ACCEPT_ENCODING' => 'gzip')
    assert_nil cached_entry(cache)

    # identity requests are a separate assembly; they have no fragments yet
    response = range_request(cache, 'bytes=0-3')
    response.body.must_equal 'abcd'
    response.headers['x-rack-cache'].must_include 'fragment_incomplete'
    assert_nil cached_entry(cache)

    # lookup keyed on the gzip variant finds its promoted representation
    gz_entry = cached_entry(cache, 'HTTP_ACCEPT_ENCODING' => 'gzip')
    refute_nil gz_entry
    gz_entry.headers['vary'].must_equal 'Accept-Encoding'
  end

  it 'does not store fragments when the option is disabled (default)' do
    backend = RangeBackend.new(FULL_BODY, '"v1"')
    @storage = Rack::Cache::Storage.new
    cache = Rack::Cache::Context.new(backend)
    response = invoke(cache, 'GET', '/download', 'HTTP_RANGE' => 'bytes=0-3')
    response.status.must_equal 206
    response.body.must_equal 'abcd'
    response.headers['x-rack-cache'].wont_include 'fragment_stored'
    cache.fragment_store.size.must_equal 0
  end
end
