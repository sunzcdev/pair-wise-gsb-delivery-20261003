# :markup: markdown
# frozen_string_literal: true
require 'domain_name'

class HTTP::CookieJar
  # This module implements SameSite-aware filtering of the cookies
  # selected for sending, based on a request context that describes
  # how the request was initiated.
  #
  # A request context is a Hash (or a SameSite::Context object) with
  # the following keys:
  #
  # :site
  # : The site (URI/URL) that initiated the request, typically the
  #   site of the top-level document.  If nil or omitted, the
  #   destination URL itself is used, which models a direct visit
  #   (e.g. typed into the address bar) and is always same-site.
  #
  # :top_level
  # : Whether the request is a top-level navigation. (default: false)
  #
  # :method
  # : The HTTP method of the request. (default: "GET")
  #
  # :redirect_chain
  # : An array of URIs/URLs that the request has been redirected
  #   through before reaching the destination. (default: [])
  #
  # ## Decision table
  #
  # A request is considered _same-site_ when the initiating site and
  # the destination URL share the same scheme and registrable domain,
  # _and_ every URL in the redirect chain is also same-site with the
  # initiating site.  A redirect chain that crosses a site boundary
  # therefore taints the whole request, even if it eventually comes
  # back to the original site.
  #
  # Given that, each cookie is filtered by its SameSite attribute as
  # follows:
  #
  #   SameSite=Strict
  #   : Sent only when the request is same-site.
  #
  #   SameSite=Lax
  #   : Sent when the request is same-site, or when it is a top-level
  #     navigation with a safe method (GET, HEAD, OPTIONS or TRACE)
  #     and the redirect chain has not crossed a site boundary.
  #
  #   SameSite=None
  #   : Sent regardless of the site context, but only if the cookie
  #     has the Secure attribute.  The Secure attribute in turn
  #     restricts the cookie to secure (HTTPS) destinations, so a
  #     SameSite=None cookie is never sent over plain HTTP.
  #
  #   (no SameSite attribute)
  #   : Always sent; the legacy behavior is preserved.
  #
  # See HTTP::CookieJar#cookies for how to enable the filtering, and
  # HTTP::CookieJar#samesite_exclusions for how to get the reasons
  # why cookies were excluded.
  module SameSite
    # HTTP methods that are considered safe (RFC 7231 4.2.1) for the
    # purpose of the SameSite=Lax top-level navigation exception.
    SAFE_METHODS = %w[GET HEAD OPTIONS TRACE].freeze

    # Reason returned when a SameSite=Strict cookie is excluded
    # because the request is cross-site.
    REASON_STRICT_CROSS_SITE = :samesite_strict_cross_site

    # Reason returned when a SameSite=Lax cookie is excluded because
    # the request is cross-site and is not a safe top-level
    # navigation.
    REASON_LAX_CROSS_SITE = :samesite_lax_cross_site

    # Reason returned when a SameSite=None cookie is excluded because
    # it lacks the Secure attribute.
    REASON_NONE_NOT_SECURE = :samesite_none_not_secure

    # A normalized request context for SameSite evaluation.  See
    # HTTP::CookieJar::SameSite for the semantics of each attribute.
    class Context
      # The destination URI of the request.
      attr_reader :uri
      # The URI of the site that initiated the request.
      attr_reader :site
      # The HTTP method of the request, in upper case.
      attr_reader :method
      # The URIs the request has been redirected through.
      attr_reader :redirect_chain

      def initialize(uri, site: nil, top_level: false, method: 'GET', redirect_chain: [])
        @uri = HTTP::Cookie::URIParser.parse(uri)
        @site = HTTP::Cookie::URIParser.parse(site || @uri)
        @top_level = top_level ? true : false
        @method = method.to_s.upcase
        @redirect_chain = redirect_chain.map { |u|
          HTTP::Cookie::URIParser.parse(u)
        }.freeze
      end

      # Whether the request is a top-level navigation.
      def top_level?
        @top_level
      end

      # Whether the request method is a safe method.
      def safe_method?
        SAFE_METHODS.include?(@method)
      end

      # Whether every URL in the redirect chain is same-site with the
      # initiating site.
      def same_site_chain?
        @redirect_chain.all? { |uri| SameSite.same_site?(@site, uri) }
      end
    end

    class << self
      # Normalizes a user given value into a SameSite::Context bound
      # to the destination `uri`.
      def context(value, uri)
        case value
        when Context
          value
        when Hash
          Context.new(uri, **value.transform_keys(&:to_sym))
        else
          raise TypeError, "invalid SameSite context: #{value.inspect}"
        end
      end

      # Tests if two URIs are same-site: same scheme and same
      # registrable domain.  IP addresses and hostnames without a
      # registrable domain are compared as whole hostnames.
      def same_site?(uri1, uri2)
        key1 = site_key(uri1)
        key2 = site_key(uri2)
        !key1.nil? && key1 == key2
      end

      # Returns nil if `cookie` should be sent in the given `context`,
      # or a Symbol explaining why it is excluded.
      def exclusion_reason(cookie, context)
        case cookie.samesite
        when nil
          nil
        when :none
          # SameSite=None is only honored with the Secure attribute,
          # which also confines the cookie to secure transports.
          cookie.secure? ? nil : REASON_NONE_NOT_SECURE
        when :strict
          same_site_request?(context) ? nil : REASON_STRICT_CROSS_SITE
        when :lax
          if same_site_request?(context) || lax_top_level_exception?(context)
            nil
          else
            REASON_LAX_CROSS_SITE
          end
        end
      end

      private

      def same_site_request?(context)
        same_site?(context.site, context.uri) && context.same_site_chain?
      end

      def lax_top_level_exception?(context)
        context.top_level? && context.safe_method? && context.same_site_chain?
      end

      def site_key(uri)
        return nil unless URI::HTTP === uri && uri.host
        domain_name = DomainName.new(uri.host)
        [uri.scheme, domain_name.domain || domain_name.hostname]
      end
    end
  end
end
