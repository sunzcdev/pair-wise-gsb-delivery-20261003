# encoding: binary
# frozen_string_literal: false

require 'rack/cache/key'

module Rack::Cache

  # In-memory accumulation of successful 206 Partial Content responses into
  # complete representations.
  #
  # A download service frequently receives scattered Range requests for the
  # same resource. Each request is answered with its own 206 response but the
  # captured byte ranges may eventually cover the whole representation. This
  # store keeps those fragments so that, once every byte is present, the
  # assembled representation can be promoted into the normal meta/entity
  # stores and served to ordinary GET requests without contacting the origin.
  #
  # Isolation (RFC 9110 14 + RFC 9111 4.1):
  #
  #   * URI           - fragments only merge for the same cache key
  #   * variants      - the request's Vary-negotiated request headers are
  #                     part of the key, so gzip/plain (etc.) never mix
  #   * representations- a strong ETag identifies the exact representation;
  #                     fragments validated against one validator can never
  #                     fill another version
  #
  # Acceptance policy for the first version:
  #
  #   * status 206 only, with a single (non-multipart) Content-Range
  #   * strong ETag validator present
  #   * complete length advertised by the "bytes START-END/TOTAL" range unit
  #   * the delivered body must contain exactly the advertised bytes
  #
  # Out-of-order and duplicate ranges merge freely, but an entry is never
  # published when overlapping bytes disagree, when a later response
  # advertises a different total length, or when the response ends early.
  #
  # The store is process-local (heap only) and is deliberately not backed by
  # the pluggable meta/entity stores; it exists to demonstrate incremental
  # fragment completion.
  class FragmentStore

    # Raised when a 206 response cannot be admitted to the fragment store.
    # +reason+ is a Symbol naming the failed acceptance rule; +chunk+ carries
    # the slurped response body (if any) so the caller can restore it before
    # forwarding the response to the client.
    class InvalidFragment < StandardError
      attr_reader :reason, :chunk
      def initialize(reason, chunk = nil)
        @reason = reason
        @chunk = chunk
        super("206 fragment rejected: #{reason}")
      end
    end

    # A single in-progress representation assembled from 206 responses.
    # Ranges are kept as a sorted list of disjoint +[start, finish, bytes]+
    # tuples covering the inclusive byte range start..finish.
    class Representation
      # Raised when an incoming fragment overlaps stored bytes with
      # different content.
      class OverlapConflict < StandardError; end

      attr_reader :etag, :total_length, :ranges

      def initialize(etag, total_length)
        @etag = etag
        @total_length = total_length
        @ranges = []
      end

      # Number of bytes currently covered (overlaps only count once).
      def covered_length
        @ranges.sum { |start, finish, _| finish - start + 1 }
      end

      # True once every byte 0...total_length is present.
      def complete?
        @ranges.length == 1 &&
          @ranges.first[0] == 0 &&
          @ranges.first[1] == @total_length - 1
      end

      # The assembled body when complete?, nil otherwise.
      def body
        complete? ? @ranges.first[2] : nil
      end

      # Add +bytes+ for the inclusive +start..finish+ range. Out-of-order and
      # duplicate ranges are accepted; raises OverlapConflict when an overlap
      # carries disagreeing bytes.
      def add(start, finish, bytes)
        # Validate every overlap against existing bytes before mutating the
        # stored ranges, so a conflicting fragment is rejected wholesale.
        @ranges.each do |existing_start, existing_finish, existing_bytes|
          overlap_start = [start, existing_start].max
          overlap_finish = [finish, existing_finish].min
          next if overlap_start > overlap_finish
          existing_slice =
            existing_bytes.byteslice(overlap_start - existing_start,
                                    overlap_finish - overlap_start + 1)
          incoming_slice =
            bytes.byteslice(overlap_start - start,
                            overlap_finish - start + 1)
          unless existing_slice == incoming_slice
            raise OverlapConflict, 'overlapping bytes differ'
          end
        end

        # Merge the new range into the sorted, disjoint range list, coalescing
        # touching/overlapping neighbors.
        merged = []
        cur_start, cur_finish, cur_bytes = start, finish, bytes
        @ranges.each do |rs, rf, rb|
          if rf < cur_start - 1
            merged << [rs, rf, rb]                       # wholly before
          elsif rs > cur_finish + 1
            merged << [cur_start, cur_finish, cur_bytes] # wholly after
            cur_start, cur_finish, cur_bytes = rs, rf, rb
          else
            new_start = [cur_start, rs].min
            new_finish = [cur_finish, rf].max
            cur_bytes = assemble(new_start, new_finish,
                                 cur_start, cur_finish, cur_bytes,
                                 rs, rf, rb)
            cur_start, cur_finish = new_start, new_finish
          end
        end
        merged << [cur_start, cur_finish, cur_bytes]
        @ranges = merged
      end

      # Gaps as an Array of +[start, finish]+ inclusive ranges not yet covered.
      def gaps
        missing = []
        cursor = 0
        @ranges.each do |start, finish, _|
          missing << [cursor, start - 1] if start > cursor
          cursor = finish + 1
        end
        missing << [cursor, @total_length - 1] if cursor < @total_length
        missing
      end

      private

      # Combine two overlapping/touching byte ranges spanning
      # +new_start..new_finish+ into one binary String. Overlapping bytes were
      # already verified equal by #add, so either range may fill the overlap.
      def assemble(new_start, new_finish, s1, f1, b1, s2, f2, b2)
        target = "\0".b * (new_finish - new_start + 1)
        target[s1 - new_start, f1 - s1 + 1] = b1
        target[s2 - new_start, f2 - s2 + 1] = b2
        target
      end
    end

    # Result of a successful #add_fragment call.
    #   key           - fragment store key the fragment belongs to
    #   representation- the Representation holding all accepted fragments
    #   complete      - whether this fragment completed the representation
    #   headers       - response headers of the completing/latest fragment
    #   chunk         - slurped response body (the 206 bytes just delivered)
    class Result < Struct.new(:key, :representation, :complete, :headers, :chunk)
      def complete?
        complete
      end
    end

    def initialize
      # base URI key -> variant signature -> ETag -> Representation
      @fragments = Hash.new { |hash, key| hash[key] = {} }
    end

    # Validate and merge one 206 response into its representation.
    #
    # Returns a Result; raises InvalidFragment when the response is not
    # admissible. The response body is slurped exactly once and returned as
    # +result.chunk+ so the caller can keep streaming it to the client.
    def add_fragment(request, response)
      chunk = slurp_body(response.body)
      etag, range_start, range_finish, total_length, headers =
        parse(response, chunk)

      base = base_key(request)
      signature = vary_signature(request, headers['vary'])
      variants = @fragments[base]
      versions = variants[signature] ||= {}
      representation = versions[etag]

      if representation
        if representation.total_length != total_length
          raise InvalidFragment.new(:total_length_mismatch, chunk)
        end
      else
        representation = Representation.new(etag, total_length)
        versions[etag] = representation
      end

      begin
        representation.add(range_start, range_finish, chunk)
      rescue Representation::OverlapConflict
        raise InvalidFragment.new(:overlap_conflict, chunk)
      end

      key = [base, signature, etag]
      Result.new(key, representation, representation.complete?, headers, chunk)
    end

    # Drop fragments for a URI/variant. When +etag+ is given only that
    # representation version is removed (e.g. after a validator change);
    # otherwise every representation version of the negotiated variant is
    # removed (e.g. once a complete entry has been published).
    def release(request, vary = nil, etag = nil)
      base = base_key(request)
      return unless variants = @fragments[base]
      signature = vary_signature(request, vary)

      if etag
        variants[signature]&.delete(etag)
      else
        variants.delete(signature)
      end

      @fragments.delete(base) if variants.empty?
    end

    # Number of representations currently tracked.
    def size
      @fragments.values.sum { |variants| variants.values.sum(&:size) }
    end

    # All Representation objects currently tracked.
    def representations
      @fragments.values.flat_map { |variants|
        variants.values.flat_map(&:values)
      }
    end

    private

    # Read a response body into a single binary String and close it.
    def slurp_body(body)
      buf = ''.b
      body.each { |part| buf << part.to_s }
      body.close if body.respond_to?(:close)
      buf
    end

    # Apply all acceptance rules. Returns
    # [etag, start, finish, total_length, headers_hash].
    def parse(response, chunk)
      unless response.status == 206
        raise InvalidFragment.new(:not_partial_content, chunk)
      end

      headers = response.headers
      cache_control = response.cache_control

      if cache_control.no_store? || cache_control.private?
        raise InvalidFragment.new(:not_cacheable, chunk)
      end

      etag = strong_etag(headers['etag'])
      unless etag
        raise InvalidFragment.new(:no_strong_etag, chunk)
      end

      content_type = headers['content-type'].to_s.downcase
      if content_type.include?('multipart/byteranges')
        raise InvalidFragment.new(:multipart_byteranges, chunk)
      end

      content_range = headers['content-range']
      match = CONTENT_RANGE_PATTERN.match(content_range.to_s)
      unless match
        raise InvalidFragment.new(:invalid_content_range, chunk)
      end

      start = match[:start].to_i
      finish = match[:finish].to_i
      total = match[:total].to_i

      if finish < start || start >= total
        raise InvalidFragment.new(:invalid_range, chunk)
      end

      expected = finish - start + 1
      if chunk.bytesize != expected
        # The response ended early (or carries more bytes than advertised).
        raise InvalidFragment.new(:length_mismatch, chunk)
      end

      [etag, start, finish, total, headers]
    end

    # Return the strong entity-tag value (quotes included) or nil when the
    # ETag header is absent or marked weak ("W/...").
    def strong_etag(value)
      return nil if value.nil?
      etag = value.strip
      return nil if etag.empty?
      # An ETag header may carry a list of validators; only a single strong
      # validator is admissible.
      return nil if etag.include?(',')
      return nil if etag =~ /\AW\//i
      return nil unless etag.start_with?('"') && etag.end_with?('"')
      etag
    end

    # bytes START-END/TOTAL with a single byte-range (RFC 9110 14.4).
    CONTENT_RANGE_PATTERN =
      /\Abytes\s+(?<start>\d+)-(?<finish>\d+)\/(?<total>\d+)\z/i

    # Canonical cache key for the request's URI.
    def base_key(request)
      keygen = request.env['rack-cache.cache_key'] || Key
      keygen.call(request)
    end

    # Canonical String of the request header values selected by Vary.
    def vary_signature(request, vary)
      return '' if vary.nil? || vary.to_s.strip.empty?
      vary.to_s.split(/[\s,]+/).map do |header|
        key = "HTTP_#{header.upcase.tr('-', '_')}"
        request.env[key].to_s
      end.join("\0")
    end
  end
end
