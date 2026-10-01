require_relative 'test_helper'
require 'rack/cache/fragment_store'

describe Rack::Cache::FragmentStore do
  before do
    @store = Rack::Cache::FragmentStore.new
  end

  def mock_request(uri = '/download', opts = {})
    Rack::Cache::Request.new(Rack::MockRequest.env_for(uri, opts))
  end

  def fragment_response(range_start, range_finish, total, bytes, headers = {})
    headers = {
      'etag' => '"v1"',
      'content-type' => 'application/octet-stream',
      'content-range' => "bytes #{range_start}-#{range_finish}/#{total}",
      'cache-control' => 'max-age=60'
    }.merge(headers)
    Rack::Cache::Response.new(206, headers, [bytes])
  end

  def add(uri_or_request = nil, response_arg = nil)
    request =
      if uri_or_request.is_a?(Rack::Cache::Request)
        uri_or_request
      elsif uri_or_request.is_a?(String)
        mock_request(uri_or_request)
      else
        mock_request
      end
    response = response_arg || yield
    @store.add_fragment(request, response)
  end

  def rejected(response, request = mock_request)
    error = assert_raises(Rack::Cache::FragmentStore::InvalidFragment) do
      @store.add_fragment(request, response)
    end
    error
  end

  describe 'acceptance' do
    it 'accepts a 206 with a strong ETag and advertised complete length' do
      result = add { fragment_response(0, 3, 8, 'Hell') }
      refute result.complete?
      result.representation.covered_length.must_equal 4
      @store.size.must_equal 1
    end

    it 'rejects non-206 responses' do
      response = Rack::Cache::Response.new(200,
        { 'etag' => '"v1"', 'content-range' => 'bytes 0-3/8' }, ['Hell'])
      rejected(response).reason.must_equal :not_partial_content
    end

    it 'rejects a weak ETag' do
      response = fragment_response(0, 3, 8, 'Hell', 'etag' => 'W/"v1"')
      rejected(response).reason.must_equal :no_strong_etag
    end

    it 'rejects a missing ETag' do
      response = fragment_response(0, 3, 8, 'Hell', 'etag' => nil)
      rejected(response).reason.must_equal :no_strong_etag
    end

    it 'rejects multipart/byteranges responses' do
      response = fragment_response(0, 3, 8, 'Hell',
        'content-type' => 'multipart/byteranges; boundary=xyz')
      rejected(response).reason.must_equal :multipart_byteranges
    end

    it 'rejects malformed content-range' do
      response = fragment_response(0, 3, 8, 'Hell',
        'content-range' => 'bytes */8')
      rejected(response).reason.must_equal :invalid_content_range
    end

    it 'rejects a response that ends early' do
      # advertises 4 bytes but only delivers 3
      response = fragment_response(0, 3, 8, 'Hel')
      error = rejected(response)
      error.reason.must_equal :length_mismatch
      error.chunk.must_equal 'Hel'
    end

    it 'rejects no-store and private fragments' do
      response = fragment_response(0, 3, 8, 'Hell',
        'cache-control' => 'no-store')
      rejected(response).reason.must_equal :not_cacheable

      response = fragment_response(0, 3, 8, 'Hell',
        'cache-control' => 'private')
      rejected(response).reason.must_equal :not_cacheable
    end

    it 'slurps a one-shot body and returns the chunk on the result' do
      io = StringIO.new('Hell')
      headers = {
        'etag' => '"v1"',
        'content-type' => 'application/octet-stream',
        'content-range' => 'bytes 0-3/8',
        'cache-control' => 'max-age=60'
      }
      response = Rack::Cache::Response.new(206, headers, io)
      result = @store.add_fragment(mock_request, response)
      result.chunk.must_equal 'Hell'
      # the one-shot body was consumed and closed during storage
      assert io.closed?
    end
  end

  describe 'assembly' do
    before do
      @total = 10
    end

    def fragment(s, f, bytes, headers = {})
      fragment_response(s, f, @total, bytes, headers)
    end

    it 'merges in-order fragments and completes on the final byte' do
      r1 = add { fragment(0, 3, '0123') }
      refute r1.complete?
      r2 = add { fragment(4, 7, '4567') }
      refute r2.complete?
      r3 = add { fragment(8, 9, '89') }
      assert r3.complete?
      r3.representation.body.must_equal '0123456789'
    end

    it 'merges fragments arriving out of order' do
      add { fragment(6, 9, '6789') }
      add { fragment(0, 2, '012') }
      result = add { fragment(3, 5, '345') }
      assert result.complete?
      result.representation.body.must_equal '0123456789'
    end

    it 'accepts duplicate identical fragments' do
      add { fragment(0, 4, '01234') }
      result = add { fragment(0, 4, '01234') }
      refute result.complete?
      result.representation.covered_length.must_equal 5
    end

    it 'accepts overlapping fragments that agree on shared bytes' do
      add { fragment(0, 4, '01234') }
      result = add { fragment(3, 9, '3456789') }
      assert result.complete?
      result.representation.body.must_equal '0123456789'
    end

    it 'accepts a suffix range resolved by the server into explicit bounds' do
      # "bytes=-4" on a 10 byte resource is delivered as bytes 6-9
      add { fragment(0, 5, '012345') }
      result = add { fragment(6, 9, '6789') }
      assert result.complete?
      result.representation.body.must_equal '0123456789'
    end

    it 'rejects overlapping fragments carrying disagreeing bytes' do
      add { fragment(0, 4, '01234') }
      response = fragment(3, 5, '999')
      error = rejected(response)
      error.reason.must_equal :overlap_conflict
      # the previously stored fragments are untouched
      representation = @store.representations.first
      representation.covered_length.must_equal 5
      representation.gaps.must_equal [[5, 9]]
    end

    it 'rejects a different advertised total length' do
      add { fragment(0, 4, '01234') }
      response = fragment_response(5, 6, 11, '56')
      rejected(response).reason.must_equal :total_length_mismatch
    end
  end

  describe 'isolation' do
    def add_with(env, response = nil)
      request = mock_request('/download', env)
      @store.add_fragment(request, response || yield)
    end

    it 'keeps different URIs separate' do
      add('/a') { fragment_response(0, 3, 8, 'aaaa') }
      add('/b') { fragment_response(0, 3, 8, 'bbbb') }
      @store.size.must_equal 2
    end

    it 'keeps Vary-negotiated variants separate' do
      add { fragment_response(0, 9, 10, '0123456789',
        'vary' => 'Accept-Encoding') }
      response = fragment_response(0, 9, 10, 'XXXXXXXXXX',
        'vary' => 'Accept-Encoding')
      add_with('HTTP_ACCEPT_ENCODING' => 'gzip') { response }
      @store.size.must_equal 2

      plain = @store.add_fragment(
        mock_request('/download'),
        fragment_response(0, 9, 10, '0123456789', 'vary' => 'Accept-Encoding'))
      assert plain.complete?
      plain.representation.body.must_equal '0123456789'
    end

    it 'keeps representation versions (strong ETags) separate' do
      add { fragment_response(0, 4, 10, '01234', 'etag' => '"v1"') }
      result = add do
        fragment_response(5, 9, 10, 'XXXXX', 'etag' => '"v2"')
      end
      @store.size.must_equal 2
      refute result.complete?
    end
  end

  describe '#release' do
    it 'removes one validator version only when an etag is given' do
      add { fragment_response(0, 4, 10, '01234', 'etag' => '"v1"') }
      add { fragment_response(0, 4, 10, '01234', 'etag' => '"v2"') }
      request = mock_request
      @store.release(request, nil, '"v1"')
      @store.size.must_equal 1
      @store.representations.first.etag.must_equal '"v2"'
    end

    it 'removes every version of the variant without an etag' do
      add { fragment_response(0, 4, 10, '01234', 'etag' => '"v1"') }
      add { fragment_response(0, 4, 10, '01234', 'etag' => '"v2"') }
      @store.release(mock_request)
      @store.size.must_equal 0
    end

    it 'does not touch fragments of other variants' do
      add { fragment_response(0, 4, 10, '01234', 'vary' => 'Accept-Encoding') }
      gz_request = mock_request('/download', 'HTTP_ACCEPT_ENCODING' => 'gzip')
      gz_response = fragment_response(0, 4, 10, '01234',
        'vary' => 'Accept-Encoding')
      @store.add_fragment(gz_request, gz_response)

      @store.release(mock_request, 'Accept-Encoding')
      @store.size.must_equal 1
    end
  end
end
