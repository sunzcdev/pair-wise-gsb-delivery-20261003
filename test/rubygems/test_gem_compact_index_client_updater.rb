# frozen_string_literal: true

require_relative "helper"
require "pathname" unless defined?(Pathname)
require "rubygems/compact_index_client"

class TestGemCompactIndexClientUpdater < Gem::TestCase
  class FakeFetcher
    attr_reader :requests

    def initialize(*responses)
      @responses = responses
      @requests = []
    end

    def call(path, headers)
      @requests << [path, headers]
      @responses.shift
    end
  end

  class FakeResponse < Gem::Net::HTTPOK
    def initialize(body, headers = {})
      super("1.1", "200", "OK")
      @fake_body = body
      headers.each {|name, value| self[name] = value }
    end

    attr_reader :fake_body
    alias_method :body, :fake_body
  end

  class FakePartialResponse < Gem::Net::HTTPPartialContent
    def initialize(body, headers = {})
      super("1.1", "206", "Partial Content")
      @fake_body = body
      headers.each {|name, value| self[name] = value }
    end

    attr_reader :fake_body
    alias_method :body, :fake_body
  end

  class FakeNotModified < Gem::Net::HTTPNotModified
    def initialize
      super("1.1", "304", "Not Modified")
    end
  end

  class FakeRangeNotSatisfiable < Gem::Net::HTTPRangeNotSatisfiable
    def initialize
      super("1.1", "416", "Range Not Satisfiable")
      @fake_body = "the requested range cannot be satisfied"
    end

    attr_reader :fake_body
    alias_method :body, :fake_body
  end

  def setup
    super

    @local_path = Pathname(@tempdir).join("versions")
    @etag_path = Pathname(@tempdir).join("versions.etag")
  end

  def digest_header(data)
    "sha-256=:#{Digest::SHA256.base64digest(data)}:"
  end

  def test_update_fetches_full_file_when_no_local_copy
    fetcher = FakeFetcher.new(FakeResponse.new("a 1.0.0\n", "ETag" => '"abc123"'))
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "abc123", @etag_path.read

    path, headers = fetcher.requests.first
    assert_equal "versions", path
    assert_empty headers
  end

  def test_update_sends_etag_and_keeps_file_on_not_modified
    @local_path.binwrite "a 1.0.0\n"
    @etag_path.binwrite "abc123"
    fetcher = FakeFetcher.new(FakeNotModified.new)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read

    _, headers = fetcher.requests.first
    assert_equal "bytes=7-", headers["Range"]
    assert_equal '"abc123"', headers["If-None-Match"]
  end

  def test_update_appends_ranged_response
    @local_path.binwrite "a 1.0.0\n"
    body = "\na 1.1.0\n"
    response = FakePartialResponse.new(body,
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\na 1.1.0\n"))
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\na 1.1.0\n", @local_path.read
    assert_equal "def456", @etag_path.read

    _, headers = fetcher.requests.first
    assert_equal "bytes=7-", headers["Range"]
  end

  def test_update_replaces_file_when_server_ignores_range
    @local_path.binwrite "stale data"
    response = FakeResponse.new("a 1.0.0\n",
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\n"))
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
  end

  def test_update_retries_with_full_request_on_bad_ranged_response
    @local_path.binwrite "a 1.0.0\n"
    bad_append = FakePartialResponse.new("\nb 1.0.0\n",
      "Repr-Digest" => digest_header("something else entirely"))
    full = FakeResponse.new("a 1.0.0\nb 1.0.0\n",
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\nb 1.0.0\n"))
    fetcher = FakeFetcher.new(bad_append, full)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\nb 1.0.0\n", @local_path.read
    assert_equal 2, fetcher.requests.size
  end

  def test_update_retries_with_full_request_when_ranged_response_does_not_overlap
    # A restored mirror can serve a regenerated index whose bytes no longer
    # line up with the local cache. The first byte of a ranged response
    # overlaps the last local byte; when it differs the range cannot be
    # resumed and the representation must be confirmed whole.
    @local_path.binwrite "a 1.0.0\n"
    other_generation = FakePartialResponse.new("X 2.0.0\n",
      "ETag" => '"regenerated"',
      "Repr-Digest" => digest_header("a 1.0.0\nX 2.0.0\n"))
    full = FakeResponse.new("a 1.0.0\n",
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\n"))
    fetcher = FakeFetcher.new(other_generation, full)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "def456", @etag_path.read
    assert_equal 2, fetcher.requests.size
    _, headers = fetcher.requests.last
    assert_nil headers["Range"]
  end

  def test_update_retries_with_full_request_on_range_not_satisfiable
    # The remote index is shorter than the local cache (e.g. a restored
    # mirror), so the range cannot be satisfied. The error body must not
    # end up in the cache; the file is refetched whole instead.
    @local_path.binwrite "a 1.0.0\na 1.1.0\n"
    full = FakeResponse.new("a 1.0.0\n",
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\n"))
    fetcher = FakeFetcher.new(FakeRangeNotSatisfiable.new, full)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "def456", @etag_path.read
    assert_equal 2, fetcher.requests.size
    _, headers = fetcher.requests.last
    assert_nil headers["Range"]
  end

  def test_update_raises_on_full_response_checksum_mismatch
    response = FakeResponse.new("a 1.0.0\n",
      "Repr-Digest" => digest_header("tampered"))
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    assert_raise Gem::CompactIndexClient::Updater::MismatchedChecksumError do
      updater.update("versions", @local_path, @etag_path)
    end

    refute @local_path.exist?
  end

  def test_update_without_etag_header_treats_response_as_update
    # Regression test for https://github.com/rubygems/bundler/issues/5463
    fetcher = FakeFetcher.new(FakeResponse.new("a 1.0.0\n"))
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
    refute @etag_path.exist?
  end

  def test_update_raises_client_error_on_invalid_gzip_response
    fetcher = Object.new
    def fetcher.call(_path, _headers)
      raise Zlib::GzipFile::Error
    end
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    e = assert_raise Gem::CompactIndexClient::Error do
      updater.update("versions", @local_path, @etag_path)
    end

    assert_includes e.message, "invalid gzip response"
  end

  def test_update_retries_with_full_request_on_blank_partial_response
    @local_path.binwrite "a 1.0.0\n"
    @etag_path.binwrite "abc123"
    blank = FakePartialResponse.new("",
      "Repr-Digest" => digest_header("a 1.0.0\nb 1.0.0\n"))
    full = FakeResponse.new("a 1.0.0\nb 1.0.0\n",
      "ETag" => '"def456"',
      "Repr-Digest" => digest_header("a 1.0.0\nb 1.0.0\n"))
    fetcher = FakeFetcher.new(blank, full)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\nb 1.0.0\n", @local_path.read
    assert_equal 2, fetcher.requests.size
    _, headers = fetcher.requests.last
    assert_nil headers["Range"]
  end

  def test_update_retry_uses_etag_written_by_another_process
    @local_path.binwrite "a 1.0.0\n"
    responses = [
      FakePartialResponse.new("\nb 1.0.0\n",
        "Repr-Digest" => digest_header("something else entirely")),
      FakeResponse.new("a 1.0.0\nb 1.0.0\n",
        "ETag" => '"def456"',
        "Repr-Digest" => digest_header("a 1.0.0\nb 1.0.0\n")),
    ]
    requests = []
    etag_path = @etag_path
    fetcher = Object.new
    fetcher.define_singleton_method(:call) do |path, headers|
      requests << [path, headers]
      # Simulate another process writing the etag during the failed first
      # request, so the retry must pick up whatever is on disk.
      etag_path.binwrite("LocalEtag") if requests.size == 1
      responses.shift
    end
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\nb 1.0.0\n", @local_path.read
    assert_equal '"LocalEtag"', requests.last[1]["If-None-Match"]
    assert_equal "def456", @etag_path.read
  end

  def test_update_with_non_utf8_response_and_ascii_default_internal
    fetcher = FakeFetcher.new(FakeResponse.new("\x8B".b))
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    old_verbose = $VERBOSE
    previous_internal_encoding = Encoding.default_internal
    begin
      $VERBOSE = false
      Encoding.default_internal = "ASCII"
      updater.update("versions", @local_path, @etag_path)
    ensure
      Encoding.default_internal = previous_internal_encoding
      $VERBOSE = old_verbose
    end

    assert_equal "\x8B".b, @local_path.binread
  end

  def test_update_parses_weak_etag
    response = FakeResponse.new("a 1.0.0\n", "ETag" => 'W/"weak1"')
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "weak1", @etag_path.read
  end

  def test_update_ignores_malformed_digest_header
    response = FakeResponse.new("a 1.0.0\n", "Repr-Digest" => "sha-256")
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
  end

  def test_update_ignores_unsupported_digest_algorithms
    response = FakeResponse.new("a 1.0.0\n",
      "Repr-Digest" => "md5=:#{Digest::MD5.base64digest("bogus")}:")
    fetcher = FakeFetcher.new(response)
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
  end

  def test_update_discards_late_response_when_concurrent_update_published
    # Two updates start from the same baseline. The old one receives a
    # ranged fragment that does not overlap the baseline (a restored
    # mirror serves a different generation) and its full re-read is
    # paused. Meanwhile the other update confirms and publishes a shorter
    # index. The old update's late response must not clobber it, and the
    # published contents keep their own verification credentials.
    @local_path.binwrite "a 1.0.0\na 1.1.0\n"
    @etag_path.binwrite "baseline-etag"

    full_request_started = Queue.new
    resume_full_request = Queue.new

    old_fetcher = Object.new
    old_fetcher.define_singleton_method(:call) do |_path, headers|
      if headers["Range"]
        FakePartialResponse.new("X 2.0.0\n",
          "ETag" => '"regenerated"',
          "Repr-Digest" => "sha-256=:#{Digest::SHA256.base64digest("a 1.0.0\na 1.1.0\nX 2.0.0\n")}:")
      else
        full_request_started << true
        resume_full_request.pop
        # The late response still carries the previous generation.
        FakeResponse.new("a 1.0.0\na 1.1.0\na 1.2.0\n",
          "ETag" => '"stale-etag"',
          "Repr-Digest" => "sha-256=:#{Digest::SHA256.base64digest("a 1.0.0\na 1.1.0\na 1.2.0\n")}:")
      end
    end

    local_path = @local_path
    etag_path = @etag_path
    old_update = Thread.new do
      Gem::CompactIndexClient::Updater.new(old_fetcher).update("versions", local_path, etag_path)
    end

    full_request_started.pop

    # The other update confirms a shorter index and publishes it together
    # with its credentials.
    new_fetcher = FakeFetcher.new(FakeResponse.new("a 1.0.0\n",
      "ETag" => '"new-etag"',
      "Repr-Digest" => digest_header("a 1.0.0\n")))
    Gem::CompactIndexClient::Updater.new(new_fetcher).update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "new-etag", @etag_path.read

    resume_full_request << true
    old_update.join

    # Readers only ever see the baseline or the confirmed shorter index,
    # and the next incremental update starts from the published one.
    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "new-etag", @etag_path.read

    next_fetcher = FakeFetcher.new(FakeNotModified.new)
    Gem::CompactIndexClient::Updater.new(next_fetcher).update("versions", @local_path, @etag_path)

    _, headers = next_fetcher.requests.first
    assert_equal "bytes=7-", headers["Range"]
    assert_equal '"new-etag"', headers["If-None-Match"]
  end

  def test_update_keeps_concurrently_published_index_on_network_error
    # Same setup, but the old update's full re-read fails after the other
    # update published: the last confirmed contents stay usable.
    @local_path.binwrite "a 1.0.0\na 1.1.0\n"
    @etag_path.binwrite "baseline-etag"

    full_request_started = Queue.new
    resume_full_request = Queue.new

    old_fetcher = Object.new
    old_fetcher.define_singleton_method(:call) do |_path, headers|
      if headers["Range"]
        FakePartialResponse.new("X 2.0.0\n",
          "Repr-Digest" => "sha-256=:#{Digest::SHA256.base64digest("a 1.0.0\na 1.1.0\nX 2.0.0\n")}:")
      else
        full_request_started << true
        resume_full_request.pop
        raise IOError, "connection reset"
      end
    end

    local_path = @local_path
    etag_path = @etag_path
    old_update = Thread.new do
      Gem::CompactIndexClient::Updater.new(old_fetcher).update("versions", local_path, etag_path)
    end
    old_update.report_on_exception = false

    full_request_started.pop

    new_fetcher = FakeFetcher.new(FakeResponse.new("a 1.0.0\n",
      "ETag" => '"new-etag"',
      "Repr-Digest" => digest_header("a 1.0.0\n")))
    Gem::CompactIndexClient::Updater.new(new_fetcher).update("versions", @local_path, @etag_path)

    resume_full_request << true
    assert_raise(IOError) { old_update.value }

    assert_equal "a 1.0.0\n", @local_path.read
    assert_equal "new-etag", @etag_path.read
  end

  def test_update_does_not_replace_file_changed_by_another_process
    # The cache file changed between the conditional request and the
    # commit, so the confirmed contents published by the other process win.
    @local_path.binwrite "a 1.0.0\n"
    local_path = @local_path
    fetcher = Object.new
    fetcher.define_singleton_method(:call) do |_path, _headers|
      local_path.binwrite "a 1.0.0\na 1.1.0\n"
      FakeResponse.new("stale 0.0.1\n",
        "ETag" => '"stale-etag"',
        "Repr-Digest" => "sha-256=:#{Digest::SHA256.base64digest("stale 0.0.1\n")}:")
    end
    updater = Gem::CompactIndexClient::Updater.new(fetcher)

    updater.update("versions", @local_path, @etag_path)

    assert_equal "a 1.0.0\na 1.1.0\n", @local_path.read
    refute @etag_path.exist?
  end
end
