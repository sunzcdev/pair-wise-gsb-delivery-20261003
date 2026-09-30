require_relative 'test_helper'
require 'rack/cache/context'

describe 'Rack::Cache::Context range requests' do
  before { setup_cache_context }
  after  { teardown_cache_context }

  # A small binary representation (10 bytes) served in three chunks so
  # that range slicing has to work across chunk boundaries.
  CHUNKS = ["\x00\x01\x02\x03".b, "\x04\x05\x06\x07".b, "\x08\x09".b]
  FULL = CHUNKS.join
  LAST_MODIFIED = 'Wed, 21 Oct 2015 07:28:00 GMT'

  def cache_binary_response(headers={})
    respond_with 200, {
      'cache-control' => 'max-age=60',
      'etag' => '"range-tag"',
      'last-modified' => LAST_MODIFIED,
      'content-type' => 'application/octet-stream'
    }.merge(headers), CHUNKS.map(&:dup)
    get '/'
    assert_equal 200, response.status
    cache.trace.must_include :store
  end

  it 'serves a single byte range from a fresh cache entry' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=2-5'

    refute app.called?
    assert_equal 206, response.status
    cache.trace.must_include :range
    response.headers['content-range'].must_equal 'bytes 2-5/10'
    response.headers['content-length'].must_equal '4'
    response.headers['accept-ranges'].must_equal 'bytes'
    response.body.must_equal FULL.byteslice(2, 4)
  end

  it 'serves ranges that span chunk boundaries' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=3-6'

    assert_equal 206, response.status
    response.headers['content-range'].must_equal 'bytes 3-6/10'
    response.body.must_equal FULL.byteslice(3, 4)
  end

  it 'serves an open-ended range through the end of the representation' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=7-'

    assert_equal 206, response.status
    response.headers['content-range'].must_equal 'bytes 7-9/10'
    response.body.must_equal FULL.byteslice(7, 3)
  end

  it 'serves a suffix range' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=-3'

    assert_equal 206, response.status
    response.headers['content-range'].must_equal 'bytes 7-9/10'
    response.body.must_equal FULL.byteslice(7, 3)
  end

  it 'serves the whole representation when the suffix exceeds its size' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=-50'

    assert_equal 206, response.status
    response.headers['content-range'].must_equal 'bytes 0-9/10'
    response.body.must_equal FULL
  end

  it 'truncates a last-byte-pos beyond the representation size' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=8-100'

    assert_equal 206, response.status
    response.headers['content-range'].must_equal 'bytes 8-9/10'
    response.body.must_equal FULL.byteslice(8, 2)
  end

  it 'serves multiple ranges as a self-consistent multipart/byteranges body' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1, 4-5, 8-'

    assert_equal 206, response.status
    response.headers['content-length'].to_i.must_equal response.body.bytesize
    boundary = response.headers['content-type'][/boundary=(\S+)/, 1]
    refute_nil boundary

    parts = response.body.split("--#{boundary}").reject { |part| part.empty? || part.start_with?('--') }
    parts.length.must_equal 3
    [[0, 1], [4, 5], [8, 9]].each_with_index do |(first, last), index|
      parts[index].must_include "Content-Type: application/octet-stream\r\n"
      parts[index].must_include "Content-Range: bytes #{first}-#{last}/10\r\n"
      payload = parts[index].split("\r\n\r\n", 2)[1].sub(/\r\n\z/, '')
      payload.must_equal FULL.byteslice(first, last - first + 1)
    end
  end

  it 'responds 416 when no range is satisfiable' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=50-60'

    assert_equal 416, response.status
    response.headers['content-range'].must_equal 'bytes */10'
    response.body.must_equal ''
  end

  it 'responds 416 for a zero-length suffix range' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=-0'

    assert_equal 416, response.status
    response.headers['content-range'].must_equal 'bytes */10'
  end

  it 'ignores a malformed range-set and serves the full representation' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=whatever'

    assert_equal 200, response.status
    response.body.must_equal FULL
  end

  it 'ignores range units other than bytes' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'items=0-1'

    assert_equal 200, response.status
    response.body.must_equal FULL
  end

  it 'honors If-Range with a matching strong entity-tag' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_RANGE' => '"range-tag"'

    assert_equal 206, response.status
    response.body.must_equal FULL.byteslice(0, 2)
  end

  it 'ignores the Range header when the If-Range entity-tag does not match' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_RANGE' => '"other-tag"'

    assert_equal 200, response.status
    response.body.must_equal FULL
  end

  it 'never matches a weak If-Range validator' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_RANGE' => 'W/"range-tag"'

    assert_equal 200, response.status
    response.body.must_equal FULL
  end

  it 'honors If-Range with a matching HTTP-date' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_RANGE' => LAST_MODIFIED

    assert_equal 206, response.status
    response.body.must_equal FULL.byteslice(0, 2)
  end

  it 'ignores the Range header when the If-Range date does not match' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_RANGE' => 'Wed, 21 Oct 2015 07:28:01 GMT'

    assert_equal 200, response.status
    response.body.must_equal FULL
  end

  it 'slices the stored representation as-is without decoding content codings' do
    cache_binary_response('content-encoding' => 'gzip')
    get '/', 'HTTP_RANGE' => 'bytes=2-5'

    assert_equal 206, response.status
    response.headers['content-encoding'].must_equal 'gzip'
    response.headers['content-range'].must_equal 'bytes 2-5/10'
    response.body.must_equal FULL.byteslice(2, 4)
  end

  it 'evaluates conditional request headers before the Range header' do
    cache_binary_response
    get '/', 'HTTP_RANGE' => 'bytes=0-1', 'HTTP_IF_NONE_MATCH' => '"range-tag"'

    assert_equal 304, response.status
    response.headers.wont_include 'content-range'
    response.body.must_equal ''
  end

  it 'ignores the Range header on HEAD requests and sends no body' do
    cache_binary_response
    head '/', 'HTTP_RANGE' => 'bytes=0-1'

    assert_equal 200, response.status
    response.headers['content-length'].must_equal '10'
    response.headers.wont_include 'content-range'
    response.body.must_equal ''
  end
end
