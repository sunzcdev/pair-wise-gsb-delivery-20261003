# frozen_string_literal: true

require "zlib"

# On RubyGems versions too old to vendor Gem::Net (when this file is loaded
# from the copy shipped inside the Bundler gem), Bundler's own
# vendored_net_http shim has already defined it before this file is loaded.
begin
  require "rubygems/vendored_net_http" unless defined?(Gem::Net::HTTP)
rescue LoadError
  require "net/http"
end

class Gem::CompactIndexClient
  # Updates the cached files on disk, keeping them in sync with the server
  # using conditional requests (ETag) and ranged requests where possible.
  class Updater
    class MismatchedChecksumError < Error
      def initialize(path, message)
        super "The checksum of /#{path} does not match the checksum provided by the server! Something is wrong. #{message}"
      end
    end

    def initialize(fetcher)
      @fetcher = fetcher
    end

    def update(remote_path, local_path, etag_path)
      # Capture the baseline before touching the network so a concurrent
      # update of the same cache can be detected when committing.
      baseline = CacheFile.baseline(local_path)
      append(remote_path, local_path, etag_path) || replace(remote_path, local_path, etag_path, baseline)
    rescue CacheFile::DigestMismatchError => e
      raise MismatchedChecksumError.new(remote_path, e.message)
    rescue Zlib::GzipFile::Error
      raise Error, "invalid gzip response while fetching /#{remote_path}"
    end

    private

    def append(remote_path, local_path, etag_path)
      return false unless local_path.file? && local_path.size.nonzero?

      CacheFile.copy(local_path) do |file|
        etag = etag_path.read.tap(&:chomp!) if etag_path.file?

        # Subtract a byte to ensure the range won't be empty.
        # Avoids 416 (Range Not Satisfiable) responses.
        response = @fetcher.call(remote_path, request_headers(etag, file.size - 1))
        break true if response.is_a?(Gem::Net::HTTPNotModified)
        # The remote file is shorter than the local copy (e.g. a restored
        # mirror serving a regenerated index), so there is nothing to
        # resume: confirm a full representation instead.
        break false if response.is_a?(Gem::Net::HTTPRangeNotSatisfiable)

        file.digests = parse_digests(response)
        # server may ignore Range and return the full response
        committed =
          if response.is_a?(Gem::Net::HTTPPartialContent)
            # A ranged response can only be resumed when its first byte
            # overlaps the last byte of the local copy. When it does not,
            # the remote file is a different generation and must be
            # confirmed whole rather than appended to.
            break false unless response.body.getbyte(0) == file.last_byte
            tail = response.body.byteslice(1..-1)
            break false unless tail && file.append(tail)
            true
          else
            file.write(response.body)
          end
        # Publish the verification credentials together with the contents
        # they belong to. When the commit was skipped because a concurrent
        # update won, its contents and credentials are kept instead.
        CacheFile.write(etag_path, etag_from_response(response)) if committed
        true
      end
    end

    # request without range header to get the full file or a 304 Not Modified
    def replace(remote_path, local_path, etag_path, baseline)
      etag = etag_path.read.tap(&:chomp!) if etag_path.file?
      response = @fetcher.call(remote_path, request_headers(etag))
      return true if response.is_a?(Gem::Net::HTTPNotModified)
      # Publish only when the cache still holds the baseline this update
      # started from. A concurrent update may have confirmed and published
      # another generation (possibly a shorter index from a restored
      # mirror) while this request was in flight, and its contents win.
      committed = CacheFile.write(local_path, response.body, parse_digests(response), baseline: baseline)
      CacheFile.write(etag_path, etag_from_response(response)) if committed
      true
    end

    def request_headers(etag, range_start = nil)
      headers = {}
      headers["Range"] = "bytes=#{range_start}-" if range_start
      headers["If-None-Match"] = %("#{etag}") if etag
      headers
    end

    def etag_from_response(response)
      return unless response["ETag"]
      etag = response["ETag"].delete_prefix("W/")
      return if etag.delete_prefix!('"') && !etag.delete_suffix!('"')
      etag
    end

    # Unwraps and returns a Hash of digest algorithms and base64 values
    # according to RFC 8941 Structured Field Values for HTTP.
    # https://www.rfc-editor.org/rfc/rfc8941#name-parsing-a-byte-sequence
    # Ignores unsupported algorithms.
    def parse_digests(response)
      return unless header = response["Repr-Digest"] || response["Digest"]
      digests = {}
      header.split(",") do |param|
        algorithm, value = param.split("=", 2)
        algorithm.strip!
        algorithm.downcase!
        next unless SUPPORTED_DIGESTS.key?(algorithm)
        next unless value
        next unless value = byte_sequence(value)
        digests[algorithm] = value
      end
      digests.empty? ? nil : digests
    end

    # Unwrap surrounding colons (byte sequence)
    # The wrapping characters must be matched or we return nil.
    # Also handles quotes because right now rubygems.org sends them.
    def byte_sequence(value)
      return if value.delete_prefix!(":") && !value.delete_suffix!(":")
      return if value.delete_prefix!('"') && !value.delete_suffix!('"')
      value
    end
  end
end
