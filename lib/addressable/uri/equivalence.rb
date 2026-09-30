# frozen_string_literal: true

module Addressable
  class URI
    ##
    # An explicit strategy for comparing URIs and generating equivalence
    # keys.
    #
    # The strategy controls three aspects of comparison:
    #
    # * whether the fragment component participates (it does not, by
    #   default);
    # * which query parameters may have their positions exchanged (none, by
    #   default);
    # * whether a parameter with a missing value (+tag+) is treated the same
    #   as one with an empty value (+tag=+) (it is not, by default).
    #
    # Everything else follows conservative, component-aware normalization:
    # scheme and host case are folded, default ports are omitted, percent
    # escapes are normalized according to each component's character set,
    # dot segments in the path are removed, and the query is compared as an
    # ordered sequence of pairs so repeated parameters are never lost.
    #
    # @example Only +tag+ may be reordered
    #   strategy = Addressable::URI::Equivalence.new(
    #     reorderable_query_params: ["tag"]
    #   )
    #   strategy.equivalent?(
    #     "http://example.com/?step=1&tag=a&tag=b",
    #     "http://example.com/?step=1&tag=b&tag=a"
    #   )
    #   #=> true
    class Equivalence
      ##
      # The URI components compared by the strategy, in the order in which
      # differences are reported.
      COMPONENTS = [
        :scheme,
        :userinfo,
        :host,
        :port,
        :path,
        :query,
        :fragment
      ].freeze

      ##
      # Sentinel used in equivalence keys for a parameter with no value at
      # all (no <code>=</code> in the pair).
      MISSING_VALUE = [:missing].freeze

      ##
      # Canonical value used for both missing and empty values when the
      # strategy treats them as equivalent.
      EMPTY_VALUE = [:value, ""].freeze

      ##
      # @param [Boolean] fragment
      #   When +true+, the fragment participates in comparison and in the
      #   equivalence key. Defaults to +false+, treating
      #   +http://example.com/a+ and +http://example.com/a#x+ as equivalent.
     # @param [Array<String, Symbol>] reorderable_query_params
      #   The names of query parameters whose positions may be exchanged.
      #   Every occurrence of a named parameter is treated as one multiset
      #   (so repeated values still count, but order does not matter), while
      #   all other parameters keep their relative order. Defaults to an
      #   empty list, meaning the order of every parameter is significant.
      # @param [Boolean] missing_value_equals_empty_value
      #   When +true+, a parameter without a value (+tag+) is treated as
      #   equivalent to a parameter with an empty value (+tag=+). Defaults
      #   to +false+.
      def initialize(
        fragment: false,
        reorderable_query_params: [],
        missing_value_equals_empty_value: false
      )
        @fragment = !!fragment
        @reorderable_query_params =
          reorderable_query_params.map { |name| name.to_s }.freeze
        @reorderable_query_param_set =
          @reorderable_query_params.to_set.freeze
        @missing_value_equals_empty_value =
          !!missing_value_equals_empty_value
        freeze
      end

      # @return [Boolean] Whether the fragment participates.
      attr_reader :fragment

      # @return [Array<String>] Query parameters whose positions may be
      #   exchanged.
      attr_reader :reorderable_query_params

      # @return [Boolean] Whether missing and empty query values compare
      #   equal.
      attr_reader :missing_value_equals_empty_value

      ##
      # Determines whether two URIs are equivalent under this strategy.
      #
      # @param [String, Addressable::URI, #to_str] uri_a
      # @param [String, Addressable::URI, #to_str] uri_b
      #
      # @return [Boolean]
      def equivalent?(uri_a, uri_b)
        difference(uri_a, uri_b).nil?
      end

      ##
      # Compares two URIs and reports the first component in which they
      # differ.
      #
      # @param [String, Addressable::URI, #to_str] uri_a
      # @param [String, Addressable::URI, #to_str] uri_b
      #
      # @return [Symbol, nil]
      #   The name of the differing component (one of {COMPONENTS}), or
      #   +nil+ if the URIs are equivalent.
      def difference(uri_a, uri_b)
        key_a = equivalence_key(uri_a)
        key_b = equivalence_key(uri_b)

        COMPONENTS.each_index do |index|
          return COMPONENTS[index] if key_a[index] != key_b[index]
        end

        nil
      end

      ##
      # Generates a stable key for a URI. Two URIs have equal keys if and
      # only if {#equivalent?} considers them equivalent, so the key is
      # suitable for grouping or deduplicating links.
      #
      # The key is a nested, frozen structure of strings, integers, symbols,
      # arrays and +nil+, and may itself be used as a Hash key.
      #
      # @param [String, Addressable::URI, #to_str] uri
      #
      # @return [Array] The equivalence key.
      def equivalence_key(uri)
        uri = coerce_uri(uri)

        [
          uri.normalized_scheme,
          uri.normalized_userinfo,
          uri.normalized_host,
          uri.normalized_port,
          uri.normalized_path,
          query_key(uri),
          @fragment ? uri.normalized_fragment : nil
        ].freeze
      end

      private

      # Preserves every repeated parameter by splitting the query into two
      # independent tracks. Fixed parameters keep their full sequence,
      # including duplicates; every occurrence of a reorderable parameter is
      # collected into one multiset and sorted. Thus the named parameters may
      # exchange positions freely, while the order of every other parameter
      # remains significant.
      def query_key(uri)
        return nil if uri.query.nil? || uri.query.empty?

        fixed_pairs = []
        reorderable_pairs = []

        pairs = parse_query_pairs(uri)
        return nil if pairs.empty?

        pairs.each do |pair|
          if @reorderable_query_param_set.include?(pair[0])
            reorderable_pairs << pair
          else
            fixed_pairs << pair
          end
        end

        [
          [:fixed, fixed_pairs.freeze].freeze,
          [:reorderable, reorderable_pairs.sort.freeze].freeze
        ].freeze
      end

      # Parses the query into [[name, value], ...] pairs without collapsing
      # duplicates. Values are represented as [:missing] or [:value,
      # String], which keeps missing and empty values distinct by default
      # and makes pairs safely comparable even when values are nil.
      def parse_query_pairs(uri)
        form_encoded =
          ["http", "https", nil].include?(uri.normalized_scheme)

        uri.query.split("&", -1).map do |pair|
          next nil if pair.empty?

          raw_name, raw_value = pair.split("=", 2)
          name = URI.unencode_component(raw_name)

          value =
            if raw_value.nil?
              if @missing_value_equals_empty_value
                EMPTY_VALUE
              else
                MISSING_VALUE
              end
            else
              raw_value = raw_value.tr("+", " ") if form_encoded
              [:value, URI.unencode_component(raw_value)].freeze
            end

          [name, value].freeze
        end.compact
      end

      def coerce_uri(uri)
        return uri if uri.is_a?(URI)
        URI.parse(uri)
      end

      private_constant :MISSING_VALUE, :EMPTY_VALUE
    end
  end
end
