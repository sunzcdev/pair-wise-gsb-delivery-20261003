# frozen_string_literal: true

#--
# Copyright (C) Bob Aman
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#++


module Addressable
  class URI
    ##
    # A bidirectional mapping between internal, absolute URIs and the public
    # URIs exposed by a gateway or reverse proxy.
    #
    # Each mapping is a pair of URI <em>mount points</em>: a location on the
    # internal network and the corresponding location on the public gateway.
    # URIs are translated by matching the longest mount point whose complete
    # components (scheme, host and default-port-aware port) and whose path
    # segments are a prefix of the URI, then swapping in the other mount point
    # while leaving the remainder of the path, plus the query and fragment,
    # untouched.
    #
    # Matching is always performed against normalized URIs, so default ports
    # and differing percent-encoding forms never cause a mismatch. User
    # information (user and password) is never accepted on a mount point and
    # never carried across a mapping.
    #
    # @example
    #   mounts = Addressable::URI::MountMap.new(
    #     "http://srv-a.internal/" => "https://gateway.example/api/"
    #   )
    #   mounts.apply("http://srv-a.internal/widgets/42?x=1#frag")
    #   #=> #<Addressable::URI URI:https://gateway.example/api/widgets/42?x=1#frag>
    #   mounts.unapply("https://gateway.example/api/widgets/42?x=1#frag")
    #   #=> #<Addressable::URI URI:http://srv-a.internal/widgets/42?x=1#frag>
    class MountMap
      ##
      # Raised when a URI provided as a mount point is not usable.
      class InvalidMountError < ArgumentError
      end

      ##
      # Raised when a URI supplied for mapping is unsuitable.
      class InvalidMappingURLError < ArgumentError
      end

      ##
      # Raised when two or more mount points match a URI equally well, or when
      # a configured set of mounts cannot form a stable bidirectional mapping.
      # Mappings never silently pick one of several candidates; the offending
      # mount points are always named.
      class AmbiguousMountError < StandardError
      end

      ##
      # Raised when no configured mount point matches the supplied URI.
      class NoMountError < StandardError
      end

      ##
      # A single internal/public mount point pairing.
      class Mount
        # @return [Addressable::URI] The normalized internal mount point.
        attr_reader :internal

        # @return [Addressable::URI] The normalized public mount point.
        attr_reader :external

        # @param [Addressable::URI] internal The internal mount point.
        # @param [Addressable::URI] external The public mount point.
        def initialize(internal, external)
          @internal = internal
          @external = external
        end

        # @return [String] A readable representation of the mount.
        def inspect
          "#<#{self.class} #{internal} => #{external}>"
        end
      end

      ##
      # Creates a new mount mapping.
      #
      # @param [Hash{String, Addressable::URI => String, Addressable::URI}] mappings
      #   Pairs of internal and public mount points. Keys are internal
      #   locations and values their public counterparts. The hash may be
      #   given in the opposite direction by passing
      #   <code>:direction => :public</code>.
      # @param [Hash] options Configuration options.
      # @option options [Symbol] :direction (:internal)
      #   When <code>:public</code>, hash keys are treated as public mount
      #   points and values as internal ones.
      def initialize(mappings = nil, options = {})
        @mounts = []
        direction = options.fetch(:direction, :internal)
        unless [:internal, :public].include?(direction)
          raise ArgumentError,
            "Invalid direction: #{direction.inspect}. " +
            "Expected :internal or :public."
        end

        (mappings || {}).each do |from, to|
          if direction == :internal
            mount(from, to)
          else
            mount(to, from)
          end
        end
        validate!
      end

      # @return [Array<Addressable::URI::MountMap::Mount>] The configured mounts.
      attr_reader :mounts

      ##
      # Registers a single mount point pairing. Exact duplicate pairings are
      # ignored.
      #
      # @param [String, Addressable::URI] internal_uri The internal location.
      # @param [String, Addressable::URI] public_uri The public location.
      #
      # @return [Addressable::URI::MountMap] The mapping, for chaining.
      #
      # @raise [InvalidMountError] if either mount point is unsuitable.
      def mount(internal_uri, public_uri)
        internal = prepare_base(internal_uri, "internal")
        external = prepare_base(public_uri, "public")
        duplicate = @mounts.any? do |existing|
          existing.internal == internal && existing.external == external
        end
        @mounts << Mount.new(internal, external) unless duplicate
        self
      end

      ##
      # Verifies that the configured mount points form an unambiguous mapping.
      #
      # Two mounts sharing an internal prefix would make {#apply} ambiguous and
      # two mounts sharing a public prefix would make {#unapply} ambiguous (the
      # classic case of one public prefix pointing at two internal addresses).
      # Nested prefixes are allowed only when they nest in the same order on
      # both sides; otherwise some links would map to an address shadowed by
      # another mount and could never be mapped back. Such configurations are
      # reported with the specific mount points involved rather than being
      # resolved by declaration order.
      #
      # @return [Addressable::URI::MountMap] The mapping, for chaining.
      #
      # @raise [AmbiguousMountError] if the configuration is not a stable
      #   bidirectional mapping.
      def validate!
        check_duplicates(:internal)
        check_duplicates(:external)
        each_pair do |a, b|
          if prefix?(a.internal, b.internal) && !prefix?(a.external, b.external)
            raise AmbiguousMountError,
              "Inconsistent mount nesting for #{describe_pair(a, b)}: " +
              "#{a.internal} nests inside #{b.internal}, but the matching " +
              "public prefixes do not nest in the same order, so some links " +
              "would be unaddressable or fail to round-trip."
          end
          if prefix?(a.external, b.external) && !prefix?(a.internal, b.internal)
            raise AmbiguousMountError,
              "Inconsistent mount nesting for #{describe_pair(a, b)}: " +
              "#{a.external} nests inside #{b.external}, but the matching " +
              "internal prefixes do not nest in the same order, so some " +
              "links would be unaddressable or fail to round-trip."
          end
        end
        self
      end

      ##
      # Translates an internal URI to its public address.
      #
      # @param [String, Addressable::URI, #to_str] uri The internal URI.
      #
      # @return [Addressable::URI] The public URI.
      #
      # @raise [NoMountError] if no mount point matches.
      # @raise [AmbiguousMountError] if several mount points match equally.
      def apply(uri)
        map_uri(uri, :internal, :external)
      end

      ##
      # Translates a public URI back to its internal address.
      #
      # @param [String, Addressable::URI, #to_str] uri The public URI.
      #
      # @return [Addressable::URI] The internal URI.
      #
      # @raise [NoMountError] if no mount point matches.
      # @raise [AmbiguousMountError] if several mount points match equally.
      def unapply(uri)
        map_uri(uri, :external, :internal)
      end

      ##
      # @return [String] A readable representation of the mapping.
      def inspect
        "#<#{self.class} [#{mounts.map(&:inspect).join(", ")}]>"
      end

      private

      # Validates and normalizes a URI intended for use as a mount base.
      def prepare_base(uri, label)
        unless uri.respond_to?(:to_str) || uri.kind_of?(Addressable::URI)
          raise InvalidMountError,
            "Can't use #{uri.class} as an #{label} mount point."
        end
        parsed = uri.kind_of?(Addressable::URI) ? uri.dup : URI.parse(uri.to_str)
        unless parsed.absolute? && !parsed.host.to_s.empty?
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} must be an absolute, " +
            "host-based URI."
        end
        if parsed.user || parsed.password
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} may not contain user " +
            "information; credentials must not be mounted."
        end
        if parsed.query
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} may not contain a query."
        end
        if parsed.fragment
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} may not contain a fragment."
        end

        raw_segments = meaningful_segments(parsed.path.to_s)
        # A single trailing slash is an acceptable directory spelling, so let
        # the normalization step below decide about a trailing empty segment.
        raw_segments.pop if raw_segments.last == ""
        if raw_segments.include?(".") || raw_segments.include?("..") ||
            raw_segments.any?(&:empty?)
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} may not contain dot or " +
            "empty path segments."
        end

        normalized = parsed.normalize
        segments = path_segments(normalized)
        # A trailing slash is an acceptable spelling of a directory mount
        # point, but it is not a distinct mount point.
        trailing_slash = segments.length > 1 && segments.last.empty?
        segments.pop if trailing_slash
        if segments.include?(".") || segments.include?("..") ||
            segments.any?(&:empty?)
          raise InvalidMountError,
            "The #{label} mount point #{uri.inspect} may not contain dot or " +
            "empty path segments."
        end

        if trailing_slash
          normalized = normalized.dup
          normalized.path = segments.empty? ? Addressable::URI::SLASH :
            Addressable::URI::SLASH + segments.join(Addressable::URI::SLASH)
        end
        normalized
      end

      # The meaningful path segments of a raw or normalized path, discarding
      # the leading segment produced by a leading slash.
      def meaningful_segments(path)
        path = path[1..-1] if path.start_with?(Addressable::URI::SLASH)
        return [] if path.nil? || path.empty?
        path.split(Addressable::URI::SLASH, -1)
      end

      # The meaningful path segments of a normalized, host-based URI.
      # The leading empty segment caused by the leading slash is discarded.
      def path_segments(uri)
        meaningful_segments(uri.path.to_s)
      end

      # The key used to compare complete URI components, ignoring the path.
      # Default ports are normalized away, so :80 and an omitted port compare
      # equal.
      def origin_key(uri)
        [uri.normalized_scheme, uri.normalized_host, uri.normalized_port]
      end

      # The full identity of a mount point: its components plus path prefix.
      def mount_key(uri)
        [origin_key(uri), canonical_segments(uri)]
      end

      # Path segments with an optional trailing slash spelling removed.
      def canonical_segments(uri)
        segments = path_segments(uri)
        segments.pop if segments.length > 1 && segments.last.empty?
        segments
      end

      # True if the complete components of +possible+ equal those of +base+
      # and the path segments of +base+ are a prefix of those of +possible+.
      def prefix?(base, possible)
        return false unless origin_key(base) == origin_key(possible)
        base_segments = canonical_segments(base)
        actual_segments = path_segments(possible)
        return false unless actual_segments.length >= base_segments.length
        actual_segments.first(base_segments.length) == base_segments
      end

      def check_duplicates(side)
        grouped = @mounts.group_by { |mount| mount_key(mount.public_send(side)) }
        grouped.each_value do |group|
          next if group.length < 2
          other_side = side == :internal ? :external : :internal
          distinct = group.map { |mount| mount.public_send(other_side) }.uniq
          next if distinct.length < 2
          action = side == :internal ? "apply" : "unapply"
          raise AmbiguousMountError,
            "Ambiguous mount configuration: #{group.map(&:inspect).join(", ")} " +
            "share the #{side} prefix #{group.first.public_send(side)} but " +
            "point at different #{other_side} locations, so #{action} would " +
            "not have a unique result."
        end
      end

      def each_pair
        @mounts.each_with_index do |a, i|
          @mounts[(i + 1)..-1].each { |b| yield a, b }
        end
      end

      def describe_pair(a, b)
        "(#{a.internal} => #{a.external}) and (#{b.internal} => #{b.external})"
      end

      def map_uri(uri, source_side, target_side)
        unless uri.respond_to?(:to_str) || uri.kind_of?(Addressable::URI)
          raise InvalidMappingURLError,
            "Can't map #{uri.class}; expected a URI."
        end
        parsed = uri.kind_of?(Addressable::URI) ? uri.dup : URI.parse(uri.to_str)
        unless parsed.absolute? && !parsed.host.to_s.empty?
          raise InvalidMappingURLError,
            "Cannot map relative or host-less URI: #{uri.inspect}."
        end
        if parsed.user || parsed.password
          # Credentials must never be relayed to the other side of a mapping;
          # refuse rather than strip them silently.
          raise InvalidMappingURLError,
            "Cannot map #{uri.inspect}: user information must not be carried " +
            "across a mount mapping."
        end

        normalized = parsed.normalize
        candidates = @mounts.select do |mount|
          prefix?(mount.public_send(source_side), normalized)
        end

        if candidates.empty?
          raise NoMountError,
            "No mount point matches #{normalized} on the " +
            "#{source_side == :internal ? "internal" : "public"} side."
        end

        longest = candidates.map do |mount|
          canonical_segments(mount.public_send(source_side)).length
        end.max
        winners = candidates.select do |mount|
          canonical_segments(mount.public_send(source_side)).length == longest
        end
        if winners.length > 1
          raise AmbiguousMountError,
            "Multiple mount points match #{normalized}: " +
            "#{winners.map(&:inspect).join(", ")}. Refine the configuration " +
            "so the longest matching prefix is unique."
        end

        mount = winners.first
        source_base = mount.public_send(source_side)
        target_base = mount.public_send(target_side)

        source_segments = canonical_segments(source_base)
        remaining_segments =
          path_segments(normalized)[source_segments.length..-1] || []
        target_segments = canonical_segments(target_base) + remaining_segments
        target_path = if target_segments.empty?
          Addressable::URI::SLASH.dup
        else
          Addressable::URI::SLASH +
            target_segments.join(Addressable::URI::SLASH)
        end

        URI.new(
          :scheme => target_base.normalized_scheme,
          :host => target_base.normalized_host,
          :port => target_base.normalized_port,
          # Credentials from the source URI are deliberately omitted.
          :path => target_path,
          :query => normalized.normalized_query,
          :fragment => normalized.normalized_fragment
        )
      end
    end
  end
end
