# frozen_string_literal: true
require 'mail/utilities'

module Mail
  # A scoped view over a multipart/related container (RFC 2387).
  class RelatedResources
    # Raised when a Content-ID is claimed by more than one resource within
    # the same multipart/related scope.
    class AmbiguousResourceError < StandardError
      attr_reader :cid, :parts

      def initialize(cid, parts)
        @cid = cid
        @parts = parts
        super "Content-ID <#{cid}> is claimed by #{parts.size} resources in the same multipart/related scope"
      end
    end

    # Matches cid: URLs in body text without requiring an HTML parser.
    CID_REFERENCE_PATTERN = /cid:([^\s"'<>\[\]()]+)/i

    # The multipart/related Mail::Message or Mail::Part this view is scoped to.
    attr_reader :container

    def initialize(container)
      @container = container
    end

    # The normalized value of the container's Content-Type +start+ parameter,
    # or nil when the parameter is absent.
    def start
      normalize_cid(container.content_type_parameters && container.content_type_parameters['start'])
    end

    # The root body part of this scope: the part whose Content-ID matches the
    # +start+ parameter, or the first part when +start+ is absent (RFC 2387).
    # Returns nil when +start+ names a part that does not exist in this scope.
    def root
      return @root if defined?(@root)
      @root = if start
        container.parts.find { |part| normalize_cid(part.content_id) == start }
      else
        container.parts.first
      end
    end

    # The Content-IDs referenced by the root body, in order of first
    # appearance, without duplicates. Nested multipart/related containers are
    # not descended into: their bodies belong to their own scope.
    def references
      @references ||= begin
        refs = []
        scan_references(root, refs) if root
        refs.uniq
      end
    end

    # The resource parts of this scope: direct children of the container that
    # carry a Content-ID, excluding the root body part. Parts inside nested
    # multipart/related containers belong to the nested scope, not this one.
    def resources
      @resources ||= container.parts.select { |part| !part.equal?(root) && !normalize_cid(part.content_id).nil? }
    end

    # Resource parts whose Content-ID is referenced by the root body.
    def referenced
      resources.select { |part| references.include?(normalize_cid(part.content_id)) }
    end

    # Resource parts never referenced by the root body.
    def unreferenced
      resources - referenced
    end

    # Content-IDs referenced by the root body for which this scope holds no
    # resource.
    def dangling_references
      references - resources.map { |part| normalize_cid(part.content_id) }
    end

    # A hash of Content-ID => parts for IDs claimed by more than one resource
    # within this same scope. Such IDs cannot be resolved unambiguously.
    def ambiguous
      resources.group_by { |part| normalize_cid(part.content_id) }.select { |_, parts| parts.size > 1 }
    end

    # Resolves a Content-ID (with or without angle brackets or a +cid:+ prefix)
    # to the resource part of this scope. Returns nil when no resource matches
    # and raises AmbiguousResourceError when the ID is claimed by more than
    # one resource in this scope.
    def resolve(cid)
      normalized = normalize_cid(cid)
      matches = resources.select { |part| normalize_cid(part.content_id) == normalized }
      raise AmbiguousResourceError.new(normalized, matches) if matches.size > 1
      matches.first
    end

    # RelatedResources views for each multipart/related part nested directly
    # inside this container. Each nested view resolves Content-IDs in its own
    # scope, isolated from this one.
    def nested
      container.parts.select { |part| part.multipart_related? }.map { |part| self.class.new(part) }
    end

    private

    def normalize_cid(cid)
      return nil if cid.nil?
      cid = Mail::Utilities.unbracket(cid.to_s.strip.sub(/\Acid:/i, '')).strip
      cid.empty? ? nil : cid
    end

    def scan_references(part, refs)
      return if part.nil?
      if part.multipart?
        # Nested multipart/related containers are scopes of their own.
        return if !part.equal?(root) && part.multipart_related?
        part.parts.each { |sub_part| scan_references(sub_part, refs) }
      else
        part.decoded.to_s.scan(CID_REFERENCE_PATTERN) do |match|
          refs << normalize_cid(match.first)
        end
      end
    end
  end
end
