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


# Avoid a circular require warning: this file is also required from the
# bottom of addressable/uri.rb, after Addressable::URI has been defined.
require "addressable/uri" unless defined?(Addressable::URI)

module Addressable
  class URI
    ##
    # A structured view of a single URI path segment, consisting of a
    # segment name and an ordered list of matrix (path) parameters.
    #
    # Matrix parameters are the <code>;name=value</code> pairs that may
    # follow a path segment, as in
    # <code>/books;lang=zh/42;rev=3</code>. Parameters are stored as an
    # ordered list of <code>[name, value]</code> pairs rather than a
    # Hash, so repeated parameter names are preserved in order. A
    # parameter written without an equals sign (a "flag" parameter, e.g.
    # <code>;archived</code>) has a value of <code>nil</code>, which is
    # distinct from an empty value (<code>;archived=</code>).
    #
    # Parsing and serialization always operate on the percent-encoded
    # form of the segment. An encoded semicolon (<code>%3B</code>) or
    # slash (<code>%2F</code>) is data, not structure: it never causes a
    # segment or parameter to be split, because splitting happens before
    # decoding and re-encoding happens after values are modified.
    #
    # @example
    #   segment = Addressable::URI::PathSegment.parse("books;lang=zh;lang=en")
    #   segment.name           # => "books"
    #   segment.params         # => [["lang", "zh"], ["lang", "en"]]
    #   segment["lang"] = "fr"
    #   segment.to_s           # => "books;lang=fr"
    class PathSegment
      include Enumerable

      ##
      # Characters that may not appear unencoded within a segment name:
      # anything outside of <code>pchar</code>, plus the structural
      # delimiters ";" and "/".
      NAME_CLASS = /[^#{CharacterClasses::PCHAR}]|[;\/]/.freeze

      ##
      # Additionally, "=" may not appear unencoded within a parameter
      # name, since it separates the name from the value.
      PARAM_NAME_CLASS = /[^#{CharacterClasses::PCHAR}]|[;\/=]/.freeze

      ##
      # Characters that may not appear unencoded within a parameter
      # value. A literal "=" is allowed in values because parsing splits
      # on the first "=" only.
      PARAM_VALUE_CLASS = /[^#{CharacterClasses::PCHAR}]|[;\/]/.freeze

      ##
      # Parses a single encoded path segment (the text between two
      # slashes) into a <code>PathSegment</code>. The input must not
      # contain a literal "/".
      #
      # @param [String, #to_str] segment_string The encoded segment.
      #
      # @return [Addressable::URI::PathSegment] The parsed segment.
      def self.parse(segment_string)
        if !segment_string.respond_to?(:to_str)
          raise TypeError,
            "Can't convert #{segment_string.class} into String."
        end
        # Split on the *encoded* string so that encoded delimiters such
        # as %3B and %2F are treated as data, not structure.
        parts = segment_string.to_str.split(";", -1)
        name = Addressable::URI.unencode(parts.shift)
        params = parts.map do |pair|
          if pair.include?("=")
            key, value = pair.split("=", 2)
            [
              Addressable::URI.unencode(key),
              Addressable::URI.unencode(value)
            ]
          else
            # No "=" at all: a flag parameter with a nil value, which is
            # distinct from an empty value (";name=").
            [Addressable::URI.unencode(pair), nil]
          end
        end
        new(name, params)
      end

      ##
      # @param [String, #to_s] name The decoded segment name.
      # @param [Array<Array(String, String)>] params
      #   The ordered list of <code>[name, value]</code> matrix parameter
      #   pairs. A <code>nil</code> value indicates a parameter without
      #   an equals sign.
      def initialize(name="", params=[])
        @name = name.to_s
        @params = params.map do |key, value|
          [key.to_s, value.nil? ? nil : value.to_s]
        end
      end

      ##
      # The decoded name of this segment (the part before the first
      # ";").
      #
      # @return [String] The segment name.
      attr_reader :name

      ##
      # Sets the segment name.
      #
      # @param [String, #to_s] new_name The new decoded segment name.
      def name=(new_name)
        @name = new_name.to_s
      end

      ##
      # The ordered list of matrix parameters as
      # <code>[name, value]</code> pairs. Repeated names appear once per
      # occurrence, in order. A <code>nil</code> value indicates a
      # parameter without an equals sign.
      #
      # @return [Array<Array(String, String)>] A copy of the pair list.
      def params
        @params.map(&:dup)
      end

      ##
      # Replaces all matrix parameters.
      #
      # @param [Array<Array(String, String)>] new_params
      #   The new ordered list of <code>[name, value]</code> pairs.
      def params=(new_params)
        @params = new_params.map do |key, value|
          [key.to_s, value.nil? ? nil : value.to_s]
        end
      end

      ##
      # Iterates over the parameter pairs in order.
      def each(&block)
        return enum_for(:each) unless block_given?
        @params.each(&block)
      end

      ##
      # The value of the first parameter with the given name, or
      # <code>nil</code> if there is no such parameter (or if the
      # parameter is a flag parameter without a value). Use
      # <code>key?</code> to distinguish these cases.
      #
      # @param [String, #to_s] key The parameter name.
      #
      # @return [String, nil] The parameter value.
      def [](key)
        pair = @params.find { |k, _| k == key.to_s }
        pair && pair[1]
      end

      ##
      # Whether a parameter with the given name is present, regardless
      # of its value.
      #
      # @param [String, #to_s] key The parameter name.
      #
      # @return [TrueClass, FalseClass]
      def key?(key)
        @params.any? { |k, _| k == key.to_s }
      end
      alias_method :has_param?, :key?

      ##
      # All values for the given parameter name, in order. Supports
      # repeated parameter names.
      #
      # @param [String, #to_s] key The parameter name.
      #
      # @return [Array] The values, possibly empty.
      def values(key)
        key = key.to_s
        @params.select { |k, _| k == key }.map { |_, v| v }
      end

      ##
      # Sets the parameter with the given name. If the parameter appears
      # multiple times, the first occurrence is updated in place and the
      # remaining duplicates are removed; otherwise the parameter is
      # appended. Other parameters are left untouched.
      #
      # @param [String, #to_s] key The parameter name.
      # @param [String, nil, #to_s] value The new value, or
      #   <code>nil</code> for a flag parameter without an equals sign.
      def []=(key, value)
        key = key.to_s
        value = value.nil? ? nil : value.to_s
        replaced = false
        @params = @params.each_with_object([]) do |(k, v), kept|
          if k == key
            if !replaced
              kept << [key, value]
              replaced = true
            end
          else
            kept << [k, v]
          end
        end
        @params << [key, value] if !replaced
        value
      end

      ##
      # Appends a parameter, preserving any existing parameters with the
      # same name. Use this to build repeated parameter names.
      #
      # @param [String, #to_s] key The parameter name.
      # @param [String, nil, #to_s] value The value, or <code>nil</code>
      #   for a flag parameter without an equals sign.
      #
      # @return [Addressable::URI::PathSegment] <code>self</code>.
      def add_param(key, value=nil)
        @params << [key.to_s, value.nil? ? nil : value.to_s]
        self
      end

      ##
      # Removes all parameters with the given name.
      #
      # @param [String, #to_s] key The parameter name.
      #
      # @return [Addressable::URI::PathSegment] <code>self</code>.
      def delete_param(key)
        key = key.to_s
        @params.delete_if { |k, _| k == key }
        self
      end

      ##
      # The encoded form of this segment, suitable for use between
      # slashes in a URI path. Structural delimiters within names and
      # values (";", "/", and "=" in parameter names) are percent
      # encoded; flag parameters are serialized without an equals sign.
      #
      # @return [String] The encoded segment.
      def to_s
        result = String.new
        result << Addressable::URI.encode_component(@name, NAME_CLASS)
        @params.each do |key, value|
          result << ";"
          result << Addressable::URI.encode_component(
            key, PARAM_NAME_CLASS
          )
          if !value.nil?
            result << "="
            result << Addressable::URI.encode_component(
              value, PARAM_VALUE_CLASS
            )
          end
        end
        result.force_encoding(Encoding::UTF_8)
        result
      end

      def ==(other)
        other.kind_of?(PathSegment) &&
          self.name == other.name &&
          self.params == other.params
      end
      alias_method :eql?, :==

      def hash
        [self.class, @name, @params].hash
      end

      protected

      def initialize_copy(other)
        super
        @params = @params.map(&:dup)
      end
    end
  end
end
