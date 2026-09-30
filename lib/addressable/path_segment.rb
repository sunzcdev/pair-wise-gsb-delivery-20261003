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
    # A structured view of a single path segment, consisting of a segment
    # name and an ordered list of matrix parameters.
    #
    # For example, the path <code>"/books;lang=zh/42;rev=3"</code> consists
    # of three segments: an empty leading segment (the leading slash), a
    # segment named <code>"books"</code> with the matrix parameter
    # <code>["lang", "zh"]</code>, and a segment named <code>"42"</code>
    # with the matrix parameter <code>["rev", "3"]</code>.
    #
    # Matrix parameters are stored as an ordered list of
    # <code>[key, value]</code> pairs so that repeated parameter names are
    # preserved. A parameter without an equals sign (e.g. <code>;flag</code>)
    # has a value of <code>nil</code>, which is distinct from a parameter
    # with an empty value (e.g. <code>;key=</code>), whose value is
    # <code>""</code>.
    #
    # Names, keys, and values are kept in their original percent-encoded
    # form. Parsing never decodes before splitting, so an encoded semicolon
    # (<code>%3B</code>) or slash (<code>%2F</code>) is treated as data and
    # cannot cause a segment or parameter to be split incorrectly.
    class PathSegment
      include Enumerable

      ##
      # Parses a single path segment string into a
      # <code>PathSegment</code>. The string must not contain any
      # <code>/</code> characters; split the path on <code>/</code> first.
      #
      # @param [String, #to_str] segment The encoded path segment string.
      #
      # @return [Addressable::URI::PathSegment] The parsed segment.
      def self.parse(segment)
        if !segment.respond_to?(:to_str)
          raise TypeError, "Can't convert #{segment.class} into String."
        end
        parts = segment.to_str.split(";", -1)
        name = parts.shift
        matrix_params = parts.map do |part|
          key, separator, value = part.partition("=")
          separator.empty? ? [key, nil] : [key, value]
        end
        new(name, matrix_params)
      end

      ##
      # @param [String] name The segment name, in percent-encoded form.
      # @param [Array<Array(String, String|nil)>] matrix_params
      #   An ordered list of <code>[key, value]</code> pairs. A value of
      #   <code>nil</code> indicates a parameter with no equals sign.
      def initialize(name="", matrix_params=[])
        @name = name.to_s
        self.matrix_params = matrix_params
      end

      ##
      # The segment name, in percent-encoded form.
      attr_accessor :name

      ##
      # The ordered list of matrix parameters as
      # <code>[key, value]</code> pairs. Duplicated keys are preserved.
      attr_reader :matrix_params

      ##
      # Replaces the matrix parameter list. The given list is copied, so
      # later changes to the argument do not affect this segment.
      def matrix_params=(new_params)
        @matrix_params = new_params.map do |pair|
          key, value = pair
          [key.to_s, value.nil? ? nil : value.to_s]
        end
      end

      ##
      # Iterates over the matrix parameters as
      # <code>[key, value]</code> pairs, in order.
      def each(&block)
        @matrix_params.each(&block)
      end

      ##
      # The value of the first matrix parameter with this key.
      #
      # @param [String] key The parameter name.
      #
      # @return [String, nil] The parameter value, <code>nil</code> if the
      #   parameter has no equals sign or is absent. Use
      #   <code>key?</code> to detect presence and
      #   <code>values</code> to see every occurrence.
      def [](key)
        pair = @matrix_params.find { |param_key, _| param_key == key }
        pair && pair[1]
      end

      ##
      # Sets the value of the first matrix parameter with this key,
      # replacing it in place and removing any later duplicates. Appends
      # a new parameter if the key is not present.
      #
      # @param [String] key The parameter name.
      # @param [String, nil] value The new value, or <code>nil</code> for
      #   a parameter with no equals sign.
      def []=(key, value)
        key = key.to_s
        value = value.to_s unless value.nil?
        index = @matrix_params.index { |param_key, _| param_key == key }
        if index.nil?
          @matrix_params.push([key, value])
        else
          @matrix_params[index] = [key, value]
          @matrix_params = @matrix_params.each_with_index.reject do |pair, i|
            i > index && pair[0] == key
          end.map(&:first)
        end
      end

      ##
      # Appends a matrix parameter, even if a parameter with the same
      # key already exists.
      #
      # @param [String] key The parameter name.
      # @param [String, nil] value The parameter value, or
      #   <code>nil</code> for a parameter with no equals sign.
      def add_param(key, value=nil)
        @matrix_params.push([key.to_s, value.nil? ? nil : value.to_s])
      end

      ##
      # Removes every matrix parameter with this key.
      #
      # @param [String] key The parameter name.
      #
      # @return [Array<String|nil>] The removed values, in order.
      def delete_param(key)
        key = key.to_s
        removed, kept = @matrix_params.partition { |k, _| k == key }
        @matrix_params = kept
        removed.map { |_, value| value }
      end

      ##
      # @param [String] key The parameter name.
      #
      # @return [Array<String|nil>] All values for this key, in order.
      def values(key)
        @matrix_params.select { |k, _| k == key }.map { |_, v| v }
      end

      ##
      # @param [String] key The parameter name.
      #
      # @return [Boolean] Whether a parameter with this key is present.
      def key?(key)
        @matrix_params.any? { |k, _| k == key }
      end

      ##
      # Serializes the segment back to its encoded string form,
      # e.g. <code>"books;lang=zh;flag"</code>.
      #
      # @return [String] The encoded segment string.
      def to_s
        result = @name.dup
        @matrix_params.each do |key, value|
          result << ";#{key}"
          result << "=#{value}" unless value.nil?
        end
        result
      end

      def ==(other)
        other.kind_of?(PathSegment) &&
          self.name == other.name &&
          self.matrix_params == other.matrix_params
      end
      alias_method :eql?, :==

      def hash
        [self.name, self.matrix_params].hash
      end
    end
  end
end
