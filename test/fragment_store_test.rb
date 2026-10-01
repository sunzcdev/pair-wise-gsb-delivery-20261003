require_relative 'test_helper'
require 'rack/cache/storage'

describe Rack::Cache::FragmentStore do
  def store
    @store ||= Rack::Cache::FragmentStore.new
  end

  def meta_store
    @meta_store ||= Rack::Cache::MetaStore::Heap.new
  end

  def entity_store
    @entity_store ||= Rack::Cache::EntityStore::Heap.new
  end

  def request_for(uri = '/file', env = {})
    env = Rack::MockRequest.env_for(uri, env)
    env['rack-cache.cache_key'] = Rack::Cache::Key
    Rack::Cache::Request.new(env)
  end

  def fragment_response(first, last, total, data, headers = {})
    headers = {
      'etag' => '"v1"',
      'content-type' => 'application/octet-stream',
      'content-range' => "bytes #{first}-#{last}/#{total}",
      'content-length' => (last - first + 1).to_s,
      'cache-control' => 'max-age=60',
    }.merge(headers)
    Rack::Cache::Response.new(206, headers, [data])
  end

  it 'parses single-range Content-Range with known complete length' do
    Rack::Cache::FragmentStore.parse_content_range('bytes 0-99/500').
      must_equal [0, 99, 500]
    Rack::Cache::FragmentStore.parse_content_range('  bytes 10-20/30 ').
      must_equal [10, 20, 30]
  end

  it 'rejects unknown complete length and malformed Content-Range' do
    Rack::Cache::FragmentStore.parse_content_range('bytes 0-99/*').must_be_nil
    Rack::Cache::FragmentStore.parse_content_range('bytes 100-99/500').must_be_nil
    Rack::Cache::FragmentStore.parse_content_range('bytes 10-500/500').must_be_nil
    Rack::Cache::FragmentStore.parse_content_range(nil).must_be_nil
  end

  it 'distinguishes strong from weak validators' do
    assert Rack::Cache::FragmentStore.strong_etag?('"x"')
    assert Rack::Cache::FragmentStore.strong_etag?('  "x"  ')
    assert Rack::Cache::FragmentStore.strong_etag?('""')
    refute Rack::Cache::FragmentStore.strong_etag?('W/"x"')
    refute Rack::Cache::FragmentStore.strong_etag?('w/"x"')
    refute Rack::Cache::FragmentStore.strong_etag?('  W / "x"')
    refute Rack::Cache::FragmentStore.strong_etag?('')
    refute Rack::Cache::FragmentStore.strong_etag?(nil)
  end

  it 'merges out-of-order fragments and only publishes when fully covered' do
    req = request_for('/file')
    total = 10
    body = '0123456789'

    store.store(req, fragment_response(6, 9, total, body[6..9]),
      meta_store, entity_store).must_equal :stored
    store.size.must_equal 1

    # plain GET cannot be satisfied yet: nothing was published
    meta_store.lookup(req, entity_store).must_be_nil

    store.store(req, fragment_response(0, 2, total, body[0..2]),
      meta_store, entity_store).must_equal :stored
    meta_store.lookup(req, entity_store).must_be_nil

    # duplicate / already-covered fragment
    store.store(req, fragment_response(0, 2, total, body[0..2]),
      meta_store, entity_store).must_equal :duplicate

    # final fragment bridges the gap
    result = store.store(req, fragment_response(3, 5, total, body[3..5]),
      meta_store, entity_store)
    result.must_equal :fragment_promoted
    store.size.must_equal 0

    entry = meta_store.lookup(req, entity_store)
    entry.status.must_equal 200
    entry.headers['content-length'].must_equal '10'
    entry.headers.wont_include 'content-range'
    entry.body.join.must_equal body
  end

  it 'accepts adjacent fragments that complete coverage' do
    req = request_for('/file')
    store.store(req, fragment_response(0, 4, 8, '01234'),
      meta_store, entity_store).must_equal :stored
    store.store(req, fragment_response(5, 7, 8, '567'),
      meta_store, entity_store).must_equal :fragment_promoted
    meta_store.lookup(req, entity_store).body.join.must_equal '01234567'
  end

  it 'accepts redundant overlapping fragments with identical bytes' do
    req = request_for('/file')
    store.store(req, fragment_response(0, 4, 8, '01234'),
      meta_store, entity_store).must_equal :stored
    store.store(req, fragment_response(3, 7, 8, '34567'),
      meta_store, entity_store).must_equal :fragment_promoted
    meta_store.lookup(req, entity_store).body.join.must_equal '01234567'
  end

  it 'poisons an accumulator when overlapping bytes disagree' do
    req = request_for('/file')
    store.store(req, fragment_response(0, 4, 8, '01234'),
      meta_store, entity_store)
    proc {
      store.store(req, fragment_response(3, 7, 8, 'XXX67'),
        meta_store, entity_store)
    }.must_raise Rack::Cache::FragmentStore::InvalidFragment
    # no complete entry is published
    meta_store.lookup(req, entity_store).must_be_nil
    # subsequent valid fragments can never repair it
    proc {
      store.store(req, fragment_response(5, 7, 8, '567'),
        meta_store, entity_store)
    }.must_raise Rack::Cache::FragmentStore::InvalidFragment
    meta_store.lookup(req, entity_store).must_be_nil
  end

  it 'poisons an accumulator when complete length contradicts' do
    req = request_for('/file')
    store.store(req, fragment_response(0, 4, 8, '01234'),
      meta_store, entity_store)
    proc {
      store.store(req, fragment_response(5, 8, 9, '5678'),
        meta_store, entity_store)
    }.must_raise Rack::Cache::FragmentStore::InvalidFragment
    meta_store.lookup(req, entity_store).must_be_nil
    store.size.must_equal 1
  end

  it 'rejects a truncated response without poisoning the accumulator' do
    req = request_for('/file')
    store.store(req, fragment_response(0, 4, 10, '01234'),
      meta_store, entity_store)
    # claims bytes 5-9 but delivers only 3 bytes
    truncated = fragment_response(5, 9, 10, '567')
    truncated.headers['content-length'] = '5'
    proc {
      store.store(req, truncated, meta_store, entity_store)
    }.must_raise Rack::Cache::FragmentStore::InvalidFragment
    # retry with the complete fragment still completes the entry
    store.store(req, fragment_response(5, 9, 10, '56789'),
      meta_store, entity_store).must_equal :fragment_promoted
    meta_store.lookup(req, entity_store).body.join.must_equal '0123456789'
  end

  it 'rejects weak validators, missing range metadata, and multipart responses' do
    req = request_for('/file')
    weak = fragment_response(0, 4, 10, '01234', 'etag' => 'W/"v1"')
    proc { store.store(req, weak, meta_store, entity_store) }.
      must_raise Rack::Cache::FragmentStore::InvalidFragment

    no_etag = fragment_response(0, 4, 10, '01234')
    no_etag.headers.delete('etag')
    proc { store.store(req, no_etag, meta_store, entity_store) }.
      must_raise Rack::Cache::FragmentStore::InvalidFragment

    star = fragment_response(0, 4, 10, '01234',
      'content-range' => 'bytes 0-4/*')
    proc { store.store(req, star, meta_store, entity_store) }.
      must_raise Rack::Cache::FragmentStore::InvalidFragment

    multi = fragment_response(0, 4, 10, '01234',
      'content-type' => 'multipart/byteranges; boundary=xyz')
    proc { store.store(req, multi, meta_store, entity_store) }.
      must_raise Rack::Cache::FragmentStore::InvalidFragment

    store.size.must_equal 0
  end

  it 'isolates fragments by URI and representation version' do
    req_a = request_for('/a')
    req_b = request_for('/b')
    store.store(req_a, fragment_response(0, 4, 8, 'AAAAA'),
      meta_store, entity_store)
    store.store(req_b, fragment_response(0, 4, 8, 'BBBBB'),
      meta_store, entity_store)
    store.size.must_equal 2

    # different representation versions of the same URI never merge
    v1 = request_for('/c')
    v2 = request_for('/c')
    store.store(v1, fragment_response(0, 4, 8, '01234'),
      meta_store, entity_store).must_equal :stored
    other = fragment_response(5, 7, 8, '567', 'etag' => '"v2"')
    store.store(v2, other, meta_store, entity_store).must_equal :stored
    store.size.must_equal 4

    # finishing v2 must not borrow v1 bytes: no promotion yet
    meta_store.lookup(v2, entity_store).must_be_nil
    store.store(v2, fragment_response(0, 4, 8, '01234', 'etag' => '"v2"'),
      meta_store, entity_store).must_equal :fragment_promoted
    entry = meta_store.lookup(v2, entity_store)
    entry.body.join.must_equal '01234567'
    entry.etag.must_equal '"v2"'

    # v1 accumulation remains, unmerged
    store.size.must_equal 3
  end

  it 'isolates content-negotiated variants via Vary' do
    json_req = request_for('/doc', 'HTTP_ACCEPT' => 'application/json')
    html_req = request_for('/doc', 'HTTP_ACCEPT' => 'text/html')
    json_resp = fragment_response(0, 3, 8, 'JSON', 'vary' => 'Accept')
    html_resp = fragment_response(0, 3, 8, 'HTML', 'vary' => 'Accept')
    store.store(json_req, json_resp, meta_store, entity_store)
    store.store(html_req, html_resp, meta_store, entity_store)
    store.size.must_equal 2
  end
end

describe 'Rack::Cache 206 fragment integration' do
  before do
    setup_cache_context
    @etags = { '/file' => '"v1"' }
    @bodies = { '/file' => '0123456789' }
    @poison_ranges = []
    @truncate_ranges = []
    @app = range_app
    @cache_config = proc do |c|
      c.set 'rack-cache.fragment_caching', true
      c.set 'rack-cache.storage', @storage
    end
  end

  after { teardown_cache_context }

  # Backend honoring Range; @poison_ranges corrupts the first overlapping
  # byte, @truncate_ranges ends the response before the claimed range.
  def range_app
    lambda do |env|
      req = Rack::Request.new(env)
      path = req.path
      full = @bodies[path]
      headers = {
        'content-type' => 'application/octet-stream',
        'etag' => @etags[path],
        'cache-control' => 'max-age=60',
        'accept-ranges' => 'bytes',
      }
      if (range = env['HTTP_RANGE']) &&
         m = /bytes=(\d*)-(\d*)/.match(range)
        first = m[1].empty? ? full.bytesize - m[2].to_i : m[1].to_i
        last  = m[2].empty? ? full.bytesize - 1 : m[2].to_i
        last = full.bytesize - 1 if last > full.bytesize - 1
        data = full.byteslice(first, last - first + 1).dup
        status = 206
        if @poison_ranges.include?(range)
          data[0] = data[0] == 'X' ? 'Y' : 'X'
        end
        truncate = @truncate_ranges.include?(range)
        headers['content-range'] =
          "bytes #{first}-#{last}/#{full.bytesize}" unless truncate
        headers['content-length'] =
          (truncate ? data.bytesize - 2 : data.bytesize).to_s
        data = data.byteslice(0, data.bytesize - 2) if truncate
        [status, headers, [data]]
      else
        # Full GETs are marked private so the "original flow" never
        # shadows the range-driven accumulation under test; fragments
        # themselves are public and cacheable.
        headers['cache-control'] = 'private'
        headers['content-length'] = full.bytesize.to_s
        [200, headers, [full]]
      end
    end
  end

  def range_get(path, first, last, opts = {})
    request(:get, path, :env => { 'HTTP_RANGE' => "bytes=#{first}-#{last}" }.merge(opts))
  end

  def fragmentstore
    @storage.resolve_fragmentstore_uri('heap:/', {})
  end

  def metastore
    @storage.resolve_metastore_uri('heap:/', {})
  end

  it 'does nothing with fragment caching disabled (default)' do
    @cache_config = nil
    response = range_get('/file', 0, 2)
    response.status.must_equal 206
    @caches.last.trace.wont_include :stored
    @caches.last.trace.wont_include :fragment_promoted
    fragmentstore.size.must_equal 0
  end

  it 'accumulates fragments, serves plain GETs only after promotion' do
    response = range_get('/file', 0, 2)
    response.status.must_equal 206
    response.headers['content-range'].must_equal 'bytes 0-2/10'
    @caches.last.trace.must_include :stored
    @caches.last.trace.wont_include :fragment_promoted

    # before completion, a plain GET follows the original flow to origin
    get '/file'
    response.status.must_equal 200
    @caches.last.trace.must_include :miss

    # out of order, duplicate, then final covering fragment
    range_get('/file', 6, 9)
    @caches.last.trace.must_include :stored
    range_get('/file', 0, 2) # duplicate
    @caches.last.trace.must_include :duplicate
    range_get('/file', 3, 5)
    response.status.must_equal 206
    @caches.last.trace.must_include :fragment_promoted

    # promotion moment: subsequent plain GET is a fresh cache hit with
    # the assembled body
    get '/file'
    response.status.must_equal 200
    @caches.last.trace.must_include :fresh
    response.body.must_equal '0123456789'
    response.headers.wont_include 'content-range'
    response.headers['content-length'].must_equal '10'
  end

  it 'never auto-fetches gaps and falls back to origin when incomplete' do
    range_get('/file', 0, 1)
    range_get('/file', 8, 9)
    get '/file'
    response.status.must_equal 200
    @caches.last.trace.must_include :miss
    response.body.must_equal '0123456789'
  end

  it 'refuses to publish on contradictory overlaps' do
    range_get('/file', 0, 4)
    @caches.last.trace.must_include :stored

    @poison_ranges << 'bytes=3-7'
    range_get('/file', 3, 7)
    @caches.last.trace.must_include :fragment_invalid
    # the client still receives the backend 206 response
    response.status.must_equal 206

    # completing with good bytes never promotes a poisoned representation
    range_get('/file', 5, 9)
    @caches.last.trace.must_include :fragment_invalid
    fragmentstore.size.must_equal 1

    get '/file'
    @caches.last.trace.must_include :miss
  end

  it 'refuses a truncated fragment but accepts a later retry' do
    range_get('/file', 0, 4)
    @truncate_ranges << 'bytes=5-9'
    range_get('/file', 5, 9)
    @caches.last.trace.must_include :fragment_invalid
    metastore.to_hash.size.must_equal 0
    fragmentstore.size.must_equal 1

    # healthy retry of the same range completes the entry
    @truncate_ranges.clear
    range_get('/file', 5, 9)
    @caches.last.trace.must_include :fragment_promoted

    get '/file'
    @caches.last.trace.must_include :fresh
    response.body.must_equal '0123456789'
  end

  it 'starts a new accumulation when the validator changes generation' do
    range_get('/file', 0, 4)
    @caches.last.trace.must_include :stored

    # origin rotates its validator: new representation version
    @etags['/file'] = '"v2"'
    range_get('/file', 5, 9)
    @caches.last.trace.must_include :stored
    @caches.last.trace.wont_include :fragment_promoted

    # the old-version fragments cannot fill the new version
    get '/file'
    @caches.last.trace.must_include :miss

    # finish the new version with its own fragments
    range_get('/file', 0, 4)
    @caches.last.trace.must_include :fragment_promoted

    get '/file'
    @caches.last.trace.must_include :fresh
    response.body.must_equal '0123456789'
    response.etag.must_equal '"v2"'

    # the stale v1 accumulation is still present but isolated
    fragmentstore.size.must_equal 1
  end

  it 'does not accumulate no-store fragments' do
    @cache_config = nil
    no_store_app = lambda do |env|
      status, headers, body = range_app.call(env)
      headers['cache-control'] = 'no-store' if status == 206
      [status, headers, body]
    end
    @app = no_store_app
    @cache_config = proc do |c|
      c.set 'rack-cache.fragment_caching', true
      c.set 'rack-cache.storage', @storage
    end
    range_get('/file', 0, 4)
    @caches.last.trace.must_include :fragment_uncacheable
    @caches.last.trace.wont_include :stored
  end
end
