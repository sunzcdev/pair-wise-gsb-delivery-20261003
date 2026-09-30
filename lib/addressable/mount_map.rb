# frozen_string_literal: true

module Addressable
  class URI
    ##
    # A bidirectional mapping between internal (origin) absolute URIs and
    # their public (gateway-facing) addresses.
    #
    # Services moved behind a gateway keep generating absolute links that
    # point at their private network location. A MountMap rewrites those
    # links to publicly reachable addresses (and maps links handed back by
    # clients to the internal address again):
    #
    #   mounts = Addressable::URI::MountMap.new do |m|
    #     m.mount "http://service-a.internal:8080/",
    #             "https://gateway.example.com/service-a/"
    #     m.mount "http://service-b.internal/",
    #             "https://gateway.example.com/service-b/"
    #   end
    #
    #   mounts.externalize("http://service-a.internal:8080/widgets/1?x=1#f")
    #   #=> #<Addressable::URI URI:https://gateway.example.com/service-a/widgets/1?x=1#f>
    #
    #   mounts.internalize("https://gateway.example.com/service-a/widgets/1?x=1#f")
    #   #=> #<Addressable::URI URI:http://service-a.internal:8080/widgets/1?x=1#f>
    #
    # Matching is performed on the full origin (scheme, host and port) plus
    # path prefixes that are aligned to path segment boundaries; the longest
    # matching prefix wins. Whatever follows the matched prefix, along with
    # the query and fragment, is preserved.
    #
    # Mappings must be unambiguous: registering the same public prefix twice
    # (or nested mounts whose internal/public layouts do not mirror each
    # other) raises an error at configuration time rather than silently
    # picking the first entry.
    class MountMap
      ##
      # Base error class for all MountMap errors.
      class Error < StandardError; end

      ##
      # Raised when a mount base is not a usable absolute URI, carries a
      # query/fragment component, or contains userinfo.
      class InvalidMountError < Error; end

      ##
      # Raised when a URI passed to {#externalize} or {#internalize} carries
      # userinfo. Userinfo is never copied across a mount, because doing so
      # could leak credentials to (or accept credentials from) the other
      # side of the gateway.
      class UserinfoError < Error; end

      ##
      # Raised when a URI cannot be matched against any registered mount.
      class MountNotFoundError < Error; end

      ##
      # Raised when two mounts would make a link resolve to more than one
      # destination, either at configuration time or while mapping.
      class AmbiguousMountError < Error; end

      ##
      # @private
      # A single internal/public base pair.
      Mount = Struct.new(:internal, :public, keyword_init: true)

      include Enumerable

      ##
      # @return [Array<Addressable::URI>] The registered internal base URIs,
      #   in registration order.
      attr_reader :internal_bases

      ##
      # @return [Array<Addressable::URI>] The registered public base URIs,
      #   in registration order.
      attr_reader :public_bases

      ##
      # Creates a new mount map.
      #
      # @yield [Addressable::URI::MountMap] The newly created map, so mounts
      #   can be registered with a block.
      def initialize
        @mounts = []
        @internal_bases = []
        @public_bases = []
        @by_internal = {}
        @by_public = {}
        yield self if block_given?
      end

      ##
      # Registers a mount point.
      #
      # @param [String, Addressable::URI] internal_uri
      #   The internal base URI (the address the service uses for itself).
      # @param [String, Addressable::URI] public_uri
      #   The public base URI (the gateway address clients see).
      #
      # @raise [InvalidMountError] if either base is not an absolute URI
      #   without userinfo, query and fragment.
      # @raise [AmbiguousMountError] if the pair collides with a mount that
      #   was registered earlier.
      #
      # @return [void]
      def mount(internal_uri, public_uri)
        internal = prepare_base(internal_uri, :internal)
        public = prepare_base(public_uri, :public)

        if (other = @by_internal[origin_key(internal)]&.find { |m|
             m.internal.path == internal.path
           })
          raise AmbiguousMountError,
            "Internal prefix #{internal} is already mounted at " \
            "#{other.public}; cannot also mount it at #{public}."
        end
        if (other = @by_public[origin_key(public)]&.find { |m|
             m.public.path == public.path
           })
          raise AmbiguousMountError,
            "Public prefix #{public} is already served by " \
            "#{other.internal}; cannot also serve #{internal} there."
        end

        entry = Mount.new(internal: internal, public: public)
        @mounts.each { |existing| assert_compatible!(existing, entry) }

        @mounts << entry
        @internal_bases << internal
        @public_bases << public
        (@by_internal[origin_key(internal)] ||= []) << entry
        (@by_public[origin_key(public)] ||= []) << entry
        nil
      end

      ##
      # @return [Integer] The number of registered mounts.
      def size
        @mounts.size
      end
      alias_method :length, :size

      ##
      # Iterates over the registered mount pairs.
      #
      # @yieldparam [Addressable::URI] internal The internal base URI.
      # @yieldparam [Addressable::URI] public The public base URI.
      def each
        return to_enum(__method__) unless block_given?
        @mounts.each { |entry| yield entry.internal, entry.public }
        self
      end

      ##
      # @param [String, Addressable::URI] uri A public URI.
      # @return [Boolean] Whether the URI could be mapped to an internal
      #   address (i.e. matches some public mount prefix).
      def internalizable?(uri)
        !match_for(coerce(uri), :public).nil?
      rescue MountNotFoundError, UserinfoError, InvalidURIError, TypeError
        false
      end

      ##
      # @param [String, Addressable::URI] uri An internal URI.
      # @return [Boolean] Whether the URI could be mapped to a public
      #   address (i.e. matches some internal mount prefix).
      def externalizable?(uri)
        !match_for(coerce(uri), :internal).nil?
      rescue MountNotFoundError, UserinfoError, InvalidURIError, TypeError
        false
      end

      ##
      # Rewrites an internal absolute URI into its public address.
      #
      # @param [String, Addressable::URI] uri The internal URI.
      #
      # @return [Addressable::URI] The public URI. The remaining path,
      #   query and fragment are preserved; default ports and percent
      #   encoding are normalized.
      #
      # @raise [UserinfoError] if the URI carries userinfo.
      # @raise [MountNotFoundError] if no internal mount prefix matches.
      # @see #internalize
      def externalize(uri)
        translate(uri, :internal, :public)
      end
      alias_method :to_public, :externalize

      ##
      # Rewrites a public URI handed back by a client into the internal
      # address.
      #
      # @param [String, Addressable::URI] uri The public URI.
      #
      # @return [Addressable::URI] The internal URI. The remaining path,
      #   query and fragment are preserved; default ports and percent
      #   encoding are normalized.
      #
      # @raise [UserinfoError] if the URI carries userinfo.
      # @raise [MountNotFoundError] if no public mount prefix matches.
      # @see #externalize
      def internalize(uri)
        translate(uri, :public, :internal)
      end
      alias_method :to_internal, :internalize

      private

      # Validates and normalizes a mount base URI.
      def prepare_base(uri, side)
        parsed =
          begin
            URI.parse(uri)
          rescue InvalidURIError, TypeError => e
            raise InvalidMountError,
              "Invalid #{side} mount base #{uri.inspect}: #{e.message}"
          end
        if parsed.scheme.nil? || parsed.host.nil? || parsed.host.empty?
          raise InvalidMountError,
            "Mount bases must be absolute URIs with a scheme and host: " \
            "#{parsed}"
        end
        if parsed.normalized_userinfo
          raise InvalidMountError,
            "Mount bases must not contain userinfo (it could leak across " \
            "the gateway): #{parsed}"
        end
        if parsed.query || parsed.fragment
          raise InvalidMountError,
            "Mount bases must not carry a query or fragment: #{parsed}"
        end
        normalized_base(parsed)
      end

      # A fully normalized base: scheme/host/path only, default port
      # stripped, encoding canonicalized.
      def normalized_base(uri)
        URI.new(
          :scheme => uri.normalized_scheme,
          :host => uri.normalized_host,
          :port => uri.normalized_port,
          :path => base_path(uri)
        )
      end

      # Normalized path, forced to start with a slash (bases always have an
      # authority component).
      def base_path(uri)
        path = uri.normalized_path
        path = "/#{path}" unless path.start_with?("/")
        path
      end

      # Normalizes a URI being mapped and rejects userinfo.
      def coerce(uri)
        parsed = URI.parse(uri)
        if parsed.normalized_userinfo
          raise UserinfoError,
            "Refusing to map #{parsed}: userinfo is never carried across " \
            "a mount, to avoid leaking credentials to the other side."
        end
        parsed
      end

      # Normalized origin key used to group candidate mounts.
      def origin_key(uri)
        "#{uri.normalized_scheme}://#{uri.normalized_host}" +
          (uri.normalized_port ? ":#{uri.normalized_port}" : "")
      end

      # Splits an absolute path into its segments on segment boundaries.
      #
      # The root path "/" has no segments.
      #
      # For mount bases (+base: true+) a single trailing empty segment (the
      # trailing slash of a directory prefix) is dropped, so that a prefix
      # represents the directory itself and everything beneath it.
      #
      # For URIs being mapped (+base: false+, the default) the trailing
      # empty segment is kept, so a trailing slash in a link round-trips.
      def self.segments_of(uri, base: false)
        path = uri.path.start_with?("/") ? uri.path[1..] : uri.path
        segments = path.split("/", -1)
        segments.pop if base && segments.last == ""
        segments
      end
      private_class_method :segments_of

      def segments_of(uri, base: false)
        self.class.send(:segments_of, uri, base: base)
      end

      # A mount base ending in "/" is a directory prefix (it matches itself
      # and everything beneath it); anything else is a file-like mount
      # point that only matches itself.
      def directory_prefix?(uri)
        uri.path.end_with?("/")
      end

      # Returns [mount_entry, remainder_segments] for the longest matching
      # mount, or nil. Raises if equal-length candidates disagree.
      def match_for(uri, side)
        normalized = uri.normalize
        index = side == :public ? @by_public : @by_internal
        list = index[origin_key(normalized)]
        return nil if list.nil? || list.empty?

        target = segments_of(normalized)
        matches = list.map { |entry|
          base_uri = entry.public_send(side)
          base_segments = segments_of(base_uri, base: true)
          remainder = remaining_segments(base_uri, base_segments, target)
          [entry, remainder] if remainder
        }.compact

        return nil if matches.empty?

        longest = matches.map { |_entry, remainder| remainder.length }.min
        best = matches.select { |_entry, remainder|
          remainder.length == longest
        }
        if best.length > 1
          bases = best.map { |entry, _r| entry.public_send(side) }.uniq
          raise AmbiguousMountError,
            "#{normalized} matches more than one #{side} mount prefix: " \
            "#{bases.join(', ')}"
        end
        best.first
      end

      # Returns the unmatched remainder segments when the target path
      # starts with the base on a segment boundary, or nil otherwise.
      #
      # A directory base (trailing slash) matches its own slash form and
      # everything beneath it. For a non-root directory it does not match
      # the slash-less form, since "/app" is a different resource from
      # "/app/"; the root base "/" (whose only form is "/") is the
      # exception. A file-like base (no trailing slash) matches only
      # itself, never a child or sibling path.
      def remaining_segments(base_uri, base_segments, target_segments)
        return nil unless target_segments.first(base_segments.length) ==
          base_segments

        remainder = target_segments[base_segments.length..] || []
        if directory_prefix?(base_uri)
          if remainder.empty? && !base_segments.empty?
            # Slash-less form of a non-root directory: not a match.
            nil
          else
            remainder
          end
        elsif remainder.empty?
          # Exact match on a file-like mount point.
          remainder
        else
          nil
        end
      end

      def translate(uri, from_side, to_side)
        parsed = coerce(uri)
        normalized = parsed.normalize
        entry, remainder = match_for(normalized, from_side)
        unless entry
          raise MountNotFoundError,
            "No mount maps #{normalized} (no #{from_side} prefix matches)."
        end

        destination = entry.public_send(to_side)
        path = join_path(destination.path, remainder)
        URI.new(
          :scheme => destination.normalized_scheme,
          :host => destination.normalized_host,
          :port => destination.normalized_port,
          :path => path,
          :query => normalized.query ? normalized.normalized_query : nil,
          :fragment => normalized.fragment ?
                         normalized.normalized_fragment : nil
        )
      end

      # Joins a normalized base path with the unmatched remainder segments.
      # The base is treated as a directory prefix: an empty remainder
      # reproduces the base path exactly (including its trailing slash, if
      # any); otherwise the segments are joined without a double slash.
      def join_path(base_path, remainder)
        return base_path if remainder.empty?
        prefix = base_path.end_with?("/") ?
          base_path[0..-2] : base_path
        "#{prefix}/#{remainder.join('/')}"
      end

      # Ensures two mounts cannot disagree about how any single public link
      # resolves. Nesting on one side must mirror nesting on the other.
      def assert_compatible!(a, b)
        pub = nesting(a.public, b.public)
        int = nesting(a.internal, b.internal)

        if pub.first == :equal || int.first == :equal
          # Exact duplicates were rejected above; anything reaching here is
          # the same origin+path on one side but different on the other.
          raise AmbiguousMountError, conflict_message(a, b)
        end

        return if pub.first == :none && int.first == :none
        return if pub.first == :nested && int.first == :nested &&
                  pub[1] == int[1] && pub[2] == int[2]

        raise AmbiguousMountError, conflict_message(a, b)
      end

      # Describes how the origins/paths of two bases relate.
      # [:none]                    unrelated origins or paths
      # [:equal]                   identical origin and path
      # [:nested, direction, rest] one prefixes the other on the same
      #                            origin; direction is :a_prefix or
      #                            :b_prefix and rest is the extra segments
      #                            oriented from the outer to the inner base
      #
      # Only a directory prefix (trailing slash) can contain another base;
      # a file-like base never nests.
      def nesting(x, y)
        x_key = origin_key(x)
        y_key = origin_key(y)
        x_segments = segments_of(x, base: true)
        y_segments = segments_of(y, base: true)
        return [:equal] if x_key == y_key && x.path == y.path
        return [:none] if x_key != y_key

        if directory_prefix?(x) && strict_prefix?(x_segments, y_segments)
          [:nested, :a_prefix, y_segments[x_segments.length..]]
        elsif directory_prefix?(y) &&
              strict_prefix?(y_segments, x_segments)
          [:nested, :b_prefix, x_segments[y_segments.length..]]
        else
          [:none]
        end
      end

      def strict_prefix?(shorter, longer)
        longer.length > shorter.length &&
          longer.first(shorter.length) == shorter
      end

      def conflict_message(a, b)
        "Mounts #{a.internal} -> #{a.public} and " \
        "#{b.internal} -> #{b.public} are ambiguous: their nested path " \
        "layout on the public side does not mirror the internal side, so " \
        "some links would resolve to more than one destination."
      end
    end
  end
end
