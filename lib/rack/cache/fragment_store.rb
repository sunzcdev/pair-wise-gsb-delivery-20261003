require 'digest/sha1'
require 'rack/cache/key'
require 'rack/cache/request'
require 'rack/cache/response'

module Rack::Cache

  # Accumulates successful 206 Partial Content responses for a single
  # representation until every byte of that representation has been
  # received, then "promotes" a normal, complete cache entry into the
  # configured MetaStore/EntityStore.
  #
  # First-version scope:
  #
  # * single-part responses only (multipart/byteranges are rejected);
  # * a strong ETag validator is required (weak validators are rejected);
  # * the representation's complete length must be known through the
  #   Content-Range complete-length field ("*" is rejected);
  # * fragments are isolated by request URI, Vary negotiation variant,
  #   and representation version (the strong ETag).
  #
  # Out-of-order and duplicate fragments are merged. An accumulator is
  # poisoned (and can never be published) when overlapping bytes disagree
  # or when the same representation version announces a contradictory
  # complete length. A response whose body ends before its claimed range
  # is complete is rejected without poisoning the accumulator.
  #
  # The store never satisfies requests itself: a plain GET is served from
  # the regular cache only after promotion, i.e. once every byte is
  # covered. Until then the request follows the normal miss/fetch flow;
  # gaps are never fetched automatically.
  #
  # Only the in-memory (heap) backend is provided in this version.
  class FragmentStore

    # Raised for any 206 response that cannot be accepted as a fragment
    # (missing/invalid validator, unknown or contradictory complete
    # length, multipart body, truncated response, byte conflict, ...).
    class InvalidFragment < StandardError; end

    # Parse a single-range Content-Range header value. Returns a
    # [first, last, complete_length] tuple or nil when the value does not
    # describe a byte range with a known complete length.
    def self.parse_content_range(value)
      return nil if value.nil? || value.to_s.strip.empty?
      match = /\Abytes\s+(\d+)-(\d+)\/(\d+)\z/.match(value.to_s.strip)
      return nil unless match
      first, last, complete = match.captures.map { |n| n.to_i }
      return nil if last < first
      return nil if complete <= last
      [first, last, complete]
    end

    # Return true when the ETag header value is a strong validator: it
    # must be present, non-empty, and not marked with the "W/" weakness
    # tag. An opaque-tag of "" is still considered strong.
    def self.strong_etag?(value)
      return false if value.nil? || value.to_s.strip.empty?
      value.to_s.strip !~ /\AW\s*\//i
    end

    def initialize
      # identity-key => {
      #   :etag => String, :complete_length => Integer,
      #   :segments => Array of [first, last, data],
      #   :headers => Hash, :request_env => Hash,
      #   :state => :ok | :broken
      # }
      @representations = {}
    end

    # Number of independent representations currently accumulating.
    def size
      @representations.size
    end

    # Read-only inspection of an accumulation. Returns nil when unknown.
    def representation(identity)
      rep = @representations[identity]
      rep && rep.dup
    end

    # Purge every accumulated fragment.
    def clear
      @representations.clear
      nil
    end

    # Attempt to store one successful 206 response fragment.
    #
    # Returns one of:
    # * :stored             - fragment merged, representation incomplete
    # * :duplicate          - fragment merged without adding new coverage
    # * :fragment_promoted  - every byte covered; complete entry published
    #
    # Raises InvalidFragment when the response is not acceptable.
    def store(request, response, meta_store, entity_store)
      etag = response.headers['etag']
      unless self.class.strong_etag?(etag)
        raise InvalidFragment, 'fragment requires a strong ETag validator'
      end
      etag = etag.to_s.strip

      if response.headers['content-type'].to_s =~ /\Amultipart\/byteranges\b/i
        raise InvalidFragment, 'multipart/byteranges responses are not supported'
      end

      range = self.class.parse_content_range(response.headers['content-range'])
      if range.nil?
        raise InvalidFragment,
          'fragment requires a single byte range with a known complete length'
      end
      first, last, complete_length = range

      data, body_size = slurp_body(response.body)
      # Reattach the slurped bytes so the 206 response remains deliverable
      # even when a later validation rejects it.
      response.body = [data.dup]

      claimed_size = last - first + 1
      if body_size != claimed_size
        # The response ended before (or ran past) the range it claimed.
        # The accumulator is left untouched so a later retry can succeed.
        raise InvalidFragment,
          "response body length #{body_size} does not match range size #{claimed_size}"
      end

      if (cl = response.headers['content-length']) &&
         cl.to_s.strip =~ /\A\d+\z/ && cl.to_i != body_size
        raise InvalidFragment,
          "content-length #{cl.to_i} does not match delivered body size #{body_size}"
      end

      identity = identity_for(request, response, etag)
      rep = @representations[identity]

      if rep
        if rep[:complete_length] != complete_length
          # Same representation version announcing a different total size.
          rep[:state] = :broken
          raise InvalidFragment,
            "complete length mismatch: #{rep[:complete_length]} != #{complete_length}"
        end
        if rep[:state] == :broken
          raise InvalidFragment, 'representation is already corrupted'
        end
      else
        rep = @representations[identity] = {
          :etag => etag,
          :complete_length => complete_length,
          :segments => [],
          :headers => nil,
          :request_env => nil,
          :state => :ok,
        }
      end

      verify_overlap!(rep, first, last, data)

      before = covered_size(rep)
      rep[:segments] = coalesce(rep[:segments] + [[first, last, data]])
      after = covered_size(rep)

      # Keep the latest response headers/environment for promotion.
      rep[:headers] = response.headers.dup
      rep[:request_env] = sanitized_env(request)

      if complete?(rep)
        promote(identity, rep, meta_store, entity_store)
        :fragment_promoted
      elsif after == before
        :duplicate
      else
        :stored
      end
    end

    private

    # Build the isolation key for a representation version: the request
    # URI (through the configured cache key generator), the negotiation
    # variant selected by the response Vary header, and the strong ETag.
    def identity_for(request, response, etag)
      keygen = request.env['rack-cache.cache_key'] || Key
      uri_key = keygen.call(request)
      [uri_key, variant_digest(request, response.vary), etag].join("\x1f")
    end

    # Digest of the request header values named by Vary. A missing Vary
    # header yields a constant digest so identical URIs share an entry.
    def variant_digest(request, vary)
      return Digest::SHA1.hexdigest('') if vary.nil? || vary.to_s.strip.empty?
      names = vary.to_s.split(/[\s,]+/).reject { |n| n.empty? }.sort
      payload =
        names.map do |name|
          key = "HTTP_#{name.upcase.tr('-', '_')}"
          "#{name}=#{request.env[key]}"
        end.join("\x1e")
      Digest::SHA1.hexdigest(payload)
    end

    # The Range request header describes which bytes are wanted, not
    # which representation was negotiated; never persist it on the
    # promoted full entry. HEAD byte-less requests become GET.
    def sanitized_env(request)
      env = request.env.dup
      env.delete('HTTP_RANGE')
      env['REQUEST_METHOD'] = 'GET'
      env
    end

    def slurp_body(body)
      data = String.new.force_encoding(Encoding::BINARY)
      size = 0
      body.each do |part|
        part = part.to_s
        data << part.dup.force_encoding(Encoding::BINARY)
        size += part.bytesize
      end
      body.close if body.respond_to?(:close)
      [data, size]
    end

    # Poison the accumulator when a stored range overlaps the new
    # fragment with different bytes.
    def verify_overlap!(rep, first, last, data)
      rep[:segments].each do |s_first, s_last, s_data|
        o_first = [first, s_first].max
        o_last  = [last, s_last].min
        next if o_first > o_last
        new_bytes = data.byteslice(o_first - first, o_last - o_first + 1)
        old_bytes = s_data.byteslice(o_first - s_first, o_last - o_first + 1)
        unless new_bytes == old_bytes
          rep[:state] = :broken
          raise InvalidFragment,
            "overlapping bytes #{o_first}-#{o_last} disagree for #{rep[:etag]}"
        end
      end
    end

    # Merge segments whose overlaps are known to hold identical bytes.
    # Touching/overlapping segments are stitched into one contiguous
    # segment.
    def coalesce(segments)
      segments = segments.sort_by { |first, _last, _data| first }
      segments.each_with_object([]) do |(first, last, data), merged|
        if merged.empty?
          merged << [first, last, data.dup]
        else
          p_first, p_last, p_data = merged.last
          if first <= p_last + 1
            if last > p_last
              p_data << data.byteslice(p_last - first + 1, last - p_last)
              merged[-1] = [p_first, last, p_data]
            end
          else
            merged << [first, last, data.dup]
          end
        end
      end
    end

    def covered_size(rep)
      rep[:segments].reduce(0) { |total, (first, last, _)| total + (last - first + 1) }
    end

    def complete?(rep)
      return false if rep[:state] != :ok
      return false if rep[:segments].empty?
      covered_size(rep) == rep[:complete_length] &&
        rep[:segments].first[0] == 0 &&
        rep[:segments].last[1] == rep[:complete_length] - 1
    end

    # Publish the assembled representation as a normal full cache entry.
    def promote(identity, rep, meta_store, entity_store)
      body = String.new.force_encoding(Encoding::BINARY)
      rep[:segments].each { |_first, _last, data| body << data }

      promoted_headers = rep[:headers]
      promoted_headers = promoted_headers.dup
      promoted_headers.delete('content-range')
      promoted_headers['content-length'] = rep[:complete_length].to_s
      promoted_headers['x-rack-cache-fragment'] = 'promoted'

      ignored = rep[:request_env]['rack-cache.ignore_headers']
      Array(ignored).each { |name| promoted_headers.delete(name) }

      promoted = Rack::Cache::Response.new(200, promoted_headers, [body])
      request = Rack::Cache::Request.new(rep[:request_env])
      meta_store.store(request, promoted, entity_store)
    ensure
      @representations.delete(identity)
    end

    public

    # In-memory FragmentStore backend.
    class Heap < FragmentStore
      def self.resolve(uri = nil, options = {})
        new
      end
    end

    HEAP = Heap
    MEM  = Heap
  end
end
