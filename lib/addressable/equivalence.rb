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

require 'addressable/uri'

module Addressable
  ##
  # An explicit policy for deciding whether two URIs are equivalent and for
  # deriving an equivalence key consistent with that decision.
  #
  # The policy is intentionally conservative: anything not explicitly
  # declared is compared strictly. Query parameter order is significant
  # unless a parameter is listed in `commutative_params`, the fragment
  # participates in comparison unless `ignore_fragment` is set, and a
  # parameter with no value (`?flag`) is distinct from a parameter with an
  # empty value (`?flag=`) unless `empty_value_equals_missing` is set.
  #
  # Component semantics still apply: the scheme and host are compared
  # case-insensitively, default ports are ignored, and percent-encoding
  # differences that decode to the same octets are treated as equal.
  # Duplicate query parameters are preserved (the query is parsed as an
  # ordered list of pairs, never collapsed into a Hash).
  #
  # @example
  #   policy = Addressable::Equivalence.new(commutative_params: ["tag"])
  #   policy.equivalent?(
  #     "http://example.com/p?tag=a&tag=b&step=1&step=2",
  #     "http://example.com/p?tag=b&tag=a&step=1&step=2"
  #   )
  #   #=> true
  #   policy.equivalent?(
  #     "http://example.com/p?tag=a&tag=b&step=1&step=2",
  #     "http://example.com/p?tag=a&tag=b&step=2&step=1"
  #   )
  #   #=> false
  class Equivalence
    ##
    # The components that take part in a comparison, in the order they are
    # reported by {Result#differences}.
    COMPONENTS = %i[scheme userinfo host port path query fragment].freeze

    ##
    # The outcome of comparing two URIs under a policy.
    class Result
      ##
      # @return [Array<Symbol>] The components that differed, drawn from
      #   {Equivalence::COMPONENTS}. Empty when the URIs are equivalent.
      attr_reader :differences

      def initialize(differences)
        @differences = differences.freeze
      end

      ##
      # @return [Boolean] True when no component differed.
      def equivalent?
        @differences.empty?
      end
    end

    ##
    # @return [Array<String>] Query parameter names whose values may be
    #   reordered without affecting equivalence.
    attr_reader :commutative_params

    ##
    # Creates a new equivalence policy.
    #
    # @param [Boolean] ignore_fragment Whether the fragment component is
    #   excluded from comparison and from equivalence keys. Defaults to
    #   `false` (fragments are compared).
    # @param [Array<String, Symbol>] commutative_params Names of query
    #   parameters whose occurrences may be freely reordered. All other
    #   parameters remain order-sensitive.
    # @param [Boolean] empty_value_equals_missing Whether a parameter with
    #   an empty value (`?flag=`) is treated the same as a parameter with
    #   no value at all (`?flag`). Defaults to `false`.
    def initialize(ignore_fragment: false, commutative_params: [],
                   empty_value_equals_missing: false)
      @ignore_fragment = ignore_fragment
      @commutative_params = commutative_params.map(&:to_s).freeze
      @empty_value_equals_missing = empty_value_equals_missing
    end

    ##
    # @return [Boolean] Whether the fragment is excluded from comparison.
    def ignore_fragment?
      @ignore_fragment
    end

    ##
    # @return [Boolean] Whether empty and missing parameter values are
    #   treated as the same.
    def empty_value_equals_missing?
      @empty_value_equals_missing
    end

    ##
    # Compares two URIs under this policy.
    #
    # @param [Addressable::URI, String] uri_a The first URI.
    # @param [Addressable::URI, String] uri_b The second URI.
    # @return [Result] The comparison result, including the components
    #   that differed.
    def compare(uri_a, uri_b)
      components_a = canonical_components(uri_a)
      components_b = canonical_components(uri_b)
      differences = COMPONENTS.select do |component|
        components_a[component] != components_b[component]
      end
      Result.new(differences)
    end

    ##
    # @param [Addressable::URI, String] uri_a The first URI.
    # @param [Addressable::URI, String] uri_b The second URI.
    # @return [Boolean] True when the two URIs are equivalent under this
    #   policy.
    def equivalent?(uri_a, uri_b)
      compare(uri_a, uri_b).equivalent?
    end

    ##
    # Derives an equivalence key for a URI. Two URIs have the same key if
    # and only if they are equivalent under this policy, so the key is
    # suitable for deduplicating links (e.g. as a Hash key or in a Set).
    #
    # @param [Addressable::URI, String] uri The URI to key.
    # @return [String] The equivalence key.
    def key(uri)
      components = canonical_components(uri)
      key_string = +""
      key_string << components[:scheme].to_s << ":"
      if components[:host]
        key_string << "//"
        if components[:userinfo]
          key_string << components[:userinfo] << "@"
        end
        key_string << components[:host].to_s
        if components[:port]
          key_string << ":" << components[:port].to_s
        end
      end
      key_string << components[:path].to_s
      if components[:query]
        key_string << "?" << components[:query].map do |name, value|
          encoded_name = encode_key_part(name)
          value.nil? ? encoded_name : "#{encoded_name}=#{encode_key_part(value)}"
        end.join("&")
      end
      if components[:fragment]
        key_string << "#" << components[:fragment]
      end
      key_string
    end

    private

    ##
    # Reduces a URI to the canonical per-component values used by both
    # comparison and key generation, guaranteeing they stay consistent.
    #
    # @return [Hash] A Hash mapping each component in {COMPONENTS} to its
    #   canonical value.
    def canonical_components(uri)
      uri = Addressable::URI.parse(uri) unless uri.is_a?(Addressable::URI)
      normalized = uri.normalize
      {
        :scheme => normalized.normalized_scheme,
        :userinfo => normalized.normalized_userinfo,
        :host => normalized.normalized_host,
        :port => normalized.normalized_port,
        :path => normalized.normalized_path,
        :query => canonical_query(normalized),
        :fragment => @ignore_fragment ? nil : normalized.normalized_fragment
      }
    end

    ##
    # Canonicalizes the query into an ordered list of `[name, value]`
    # pairs. Duplicate parameters are preserved. Pairs declared as
    # commutative are extracted and sorted so that their order no longer
    # matters; every other pair keeps its original relative order.
    #
    # @return [Array<Array(String, String)>, nil] The canonical pairs, or
    #   nil when the URI has no query.
    def canonical_query(uri)
      pairs = uri.query_values(Array)
      return nil if pairs.nil?
      pairs = pairs.map do |name, value|
        value = "" if value.nil? && @empty_value_equals_missing
        [name, value]
      end
      commutative, ordered = pairs.partition do |name, _value|
        @commutative_params.include?(name)
      end
      ordered + commutative.sort_by { |name, value| [name, value.to_s] }
    end

    ##
    # Encodes a query name or value for use in an equivalence key so that
    # delimiters inside values cannot collide with key syntax.
    def encode_key_part(value)
      Addressable::URI.encode_component(
        value.to_s, Addressable::URI::CharacterClasses::UNRESERVED
      )
    end
  end
end
