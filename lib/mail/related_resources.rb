# encoding: utf-8
# frozen_string_literal: true
require 'mail/utilities'

module Mail

  # = Related Resources
  #
  # A read-only view over a multipart/related container (RFC 2387) that
  # resolves the embedded resources (inline images and friends) the root
  # body part refers to through "cid:" URLs.
  #
  # The view is scoped to a single multipart/related container:
  #
  # * The root body part is selected with the container's +start+
  #   parameter, matched against the Content-ID of the parts in scope.
  #   Without a +start+ parameter (or when it does not resolve) the
  #   first part is the root, per RFC 2387.
  # * Content-IDs are resolved against the parts of this container only.
  #   A nested multipart/related container is a scope of its own, so an
  #   ID reused inside it neither conflicts with nor leaks into the
  #   enclosing scope.
  # * Duplicate Content-IDs within this one scope are genuinely
  #   ambiguous and are reported through #ambiguous.
  #
  # References are collected by scanning the root body text for "cid:"
  # URLs. External references (http:, https:, ...) are never fetched and
  # no HTML engine is involved.
  #
  #   mail = Mail.read('message.eml')
  #   view = mail.related_resources
  #   view.root         #=> the root body part
  #   view.referenced   #=> parts the root body refers to
  #   view.unreferenced #=> parts with a Content-ID nothing refers to
  #   view.dangling     #=> "cid:" references without a matching part
  #   view.ambiguous    #=> { "id" => [part, part] } duplicate IDs
  class RelatedResources

    # Matches "cid:" URLs in body text, stopping at whitespace and the
    # usual HTML/URL delimiters.
    CID_URL_PATTERN = /cid:([^\s"'<>()\[\]{}]+)/i

    # The Mail::Message or Mail::Part this view is scoped to. Expected
    # to be multipart/related, but other containers are tolerated.
    attr_reader :container

    def initialize(container)
      @container = container
    end

    # The root body part of this container: the part whose Content-ID
    # matches the +start+ parameter, or the first part when +start+ is
    # missing or does not resolve to a part in this scope.
    def root
      start = start_content_id
      if start && (part = scoped_parts.find { |p| content_id_of(p) == start })
        part
      else
        container.parts.first
      end
    end

    # All parts carrying a Content-ID that belong to this scope, except
    # the root body part itself: the root is the document, not a
    # resource.
    #
    # Multipart parts other than multipart/related (e.g.
    # multipart/alternative) are transparent: their children belong to
    # this scope. A nested multipart/related container starts a scope of
    # its own, so only the container part itself (when it carries an ID)
    # is included, never its children.
    def resources
      @resources ||= scoped_parts.reject { |part| part.equal?(root) }
    end

    # The Content-IDs the root body refers to, in order of first use.
    def references
      @references ||= scan_references(root_text)
    end

    # Scoped parts whose Content-ID is referenced by the root body.
    def referenced
      resources.select { |part| references.include?(content_id_of(part)) }
    end

    # Scoped parts whose Content-ID nothing in the root body refers to.
    def unreferenced
      resources - referenced
    end

    # Referenced Content-IDs with no matching part in this scope.
    def dangling
      known = resources.map { |part| content_id_of(part) }
      references.reject { |cid| known.include?(cid) }
    end

    # Content-IDs carried by more than one part in this scope, mapped to
    # the parts sharing them. The same ID reused inside a nested
    # multipart/related container does not appear here: only duplicates
    # within this one scope are genuinely ambiguous.
    def ambiguous
      index.select { |_cid, parts| parts.length > 1 }
    end

    # True when the given Content-ID is carried by more than one part in
    # this scope.
    def ambiguous?(cid)
      ambiguous.key?(normalize_cid(cid))
    end

    # Resolves a Content-ID (with or without angle brackets, optionally
    # percent-escaped) to a part in this scope. Returns nil when the ID
    # is unknown. When the ID is ambiguous the first matching part is
    # returned and the clash is reported through #ambiguous.
    def [](cid)
      parts = index[normalize_cid(cid)]
      parts && parts.first
    end

    private

    def index
      @index ||= resources.group_by { |part| content_id_of(part) }
    end

    def scoped_parts
      @scoped_parts ||= collect_resources(container)
    end

    def start_content_id
      params = container.content_type_parameters
      params && params['start'] ? normalize_cid(params['start']) : nil
    end

    def collect_resources(node)
      node.parts.flat_map do |part|
        if related_container?(part)
          part.has_content_id? ? [part] : []
        elsif part.multipart?
          collect_resources(part)
        else
          part.has_content_id? ? [part] : []
        end
      end
    end

    def related_container?(part)
      part.multipart? && part.sub_type.to_s.downcase == 'related'
    end

    # The decoded text of the root part. A multipart root (commonly
    # multipart/alternative) contributes the text of its leaves, stopping
    # at nested multipart/related containers, which are separate scopes.
    def root_text
      texts_for(root).join("\n")
    end

    def texts_for(part)
      if part.multipart?
        return [] if related_container?(part) && !part.equal?(root)
        part.parts.flat_map { |child| texts_for(child) }
      else
        [part.body.decoded]
      end
    end

    def scan_references(text)
      text.to_s.scan(CID_URL_PATTERN).flatten.map { |ref| normalize_cid(ref) }.uniq
    end

    def content_id_of(part)
      normalize_cid(part.content_id)
    end

    def normalize_cid(cid)
      Utilities.uri_unescape(Utilities.unbracket(cid.to_s.strip))
    end
  end
end
