# :markup: markdown
# frozen_string_literal: true

class HTTP::CookieJar
  # Describes the context of an outgoing HTTP request, used by
  # HTTP::CookieJar#cookies to filter the send set by each cookie's
  # SameSite attribute.
  #
  # A context is made of:
  #
  # :initiator
  # : The site (URI/URL) that initiated the request, e.g. the site of
  #   the page a subresource request or a navigation originates from.
  #   nil means the request has no initiator, as when the user types a
  #   URL directly into the address bar, and is always considered
  #   same-site.
  #
  # :top_level
  # : Whether the request is a top-level navigation. (default: false)
  #
  # :method
  # : The HTTP method of the request. (default: "GET")
  #
  # :redirect_chain
  # : An array of URIs/URLs already visited in the redirect chain
  #   that led to this request.  If any hop is cross-site relative to
  #   the request target, the request is considered cross-site even
  #   when the initiator is same-site (e.g. a cross-site page that
  #   redirects back to the original site).
  #
  # Two URIs are considered "same-site" when they share the same
  # scheme and the same registrable domain (eTLD+1).
  #
  # The decision table implemented by #exclusion_reason is as follows:
  #
  #   | Cookie SameSite | same-site request | cross-site, top-level | cross-site,        |
  #   |                 |                   | safe method           | otherwise          |
  #   |-----------------+-------------------+-----------------------+--------------------|
  #   | Strict          | send              | exclude               | exclude            |
  #   | Lax             | send              | send                  | exclude            |
  #   | None            | send (Secure required, see below)                            |
  #   | (unset)         | treated as Lax ("Lax-by-default")                            |
  #
  # A cookie with SameSite=None is only sent when it carries the
  # Secure attribute; combined with the ordinary secure-flag check in
  # HTTP::Cookie#valid_for_uri?, this guarantees it only ever travels
  # over a secure transport.
  class SameSiteContext
    # HTTP methods considered "safe" per RFC 9110.
    SAFE_METHODS = %w[GET HEAD OPTIONS TRACE].freeze

    # The site that initiated the request, as a URI object, or nil.
    attr_reader :initiator

    # The HTTP method of the request, upper-cased.
    attr_reader :method

    # The URIs already visited in the redirect chain, as URI objects.
    attr_reader :redirect_chain

    def initialize(initiator: nil, top_level: false, method: 'GET', redirect_chain: [])
      @initiator = initiator && HTTP::Cookie::URIParser.parse(initiator)
      @top_level = top_level ? true : false
      @method = String.try_convert(method).to_s.upcase
      @redirect_chain = redirect_chain.map { |url| HTTP::Cookie::URIParser.parse(url) }
    end

    # Wraps a given object in a SameSiteContext.  A SameSiteContext is
    # returned as-is, and a hash is used as keyword arguments.
    def self.wrap(object)
      case object
      when nil, self
        object
      when Hash
        new(**object.transform_keys(&:to_sym))
      else
        raise TypeError, "cannot convert #{object.class} into #{self}"
      end
    end

    # Tests if the request is a top-level navigation.
    def top_level?
      @top_level
    end

    # Tests if the request uses a "safe" HTTP method (GET, HEAD,
    # OPTIONS or TRACE).
    def safe_method?
      SAFE_METHODS.include?(@method)
    end

    # Tests if a request to `uri` is same-site in this context, taking
    # the initiator and every hop of the redirect chain into account.
    def same_site?(uri)
      uri = HTTP::Cookie::URIParser.parse(uri)
      return false if @initiator && !self.class.same_site_uri?(@initiator, uri)
      @redirect_chain.all? { |hop| self.class.same_site_uri?(hop, uri) }
    end

    # Tests if two URIs are same-site, i.e. share the same scheme and
    # the same registrable domain (eTLD+1).
    def self.same_site_uri?(uri1, uri2)
      uri1.scheme == uri2.scheme && site_of(uri1) == site_of(uri2)
    end

    # Returns nil if `cookie` should be sent to `uri` in this context,
    # or a symbol describing why it is excluded:
    #
    # :samesite_strict
    # : The cookie is SameSite=Strict and the request is cross-site.
    #
    # :samesite_lax
    # : The cookie is SameSite=Lax (or has no SameSite attribute) and
    #   the request is a cross-site request that is not a top-level
    #   navigation with a safe method.
    #
    # :samesite_none_secure
    # : The cookie is SameSite=None but lacks the Secure attribute
    #   required for safe transport.
    def exclusion_reason(cookie, uri)
      case cookie.samesite || :lax
      when :strict
        same_site?(uri) ? nil : :samesite_strict
      when :lax
        return nil if same_site?(uri)
        top_level? && safe_method? ? nil : :samesite_lax
      when :none
        cookie.secure? ? nil : :samesite_none_secure
      end
    end

    # Returns the registrable domain (eTLD+1) of the URI's host, or
    # the bare hostname for IP addresses and public suffixes.
    def self.site_of(uri)
      domain_name = DomainName.new(uri.host)
      domain_name.domain || domain_name.hostname
    end
    private_class_method :site_of
  end
end
