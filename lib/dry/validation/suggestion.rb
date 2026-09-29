# frozen_string_literal: true

require "dry/schema/path"
require "dry/validation/constants"

module Dry
  module Validation
    # Suggestion represents a structured fix candidate attached to a validation error
    #
    # Suggestions describe a single, explicit modification (such as removing
    # a deprecated key or replacing an invalid enum value) that *could* make
    # the input valid. They are never applied automatically - a caller must
    # pick a conflict-free set and pass it to Result#apply_suggestions.
    #
    # @example
    #   rule(:status) do
    #     unless %w[active archived].include?(value)
    #       key.failure("is invalid", suggestions: [
    #         {operation: :replace, path: [:status], value: "active"}
    #       ])
    #     end
    #   end
    #
    # @see Result#suggestions
    # @see Result#apply_suggestions
    #
    # @api public
    class Suggestion
      include ::Dry::Equalizer(:operation, :path, :value, inspect: false)

      # Supported operation types
      #
      # @api public
      OPERATIONS = %i[delete replace].freeze

      # The operation type (:delete or :replace)
      #
      # @return [Symbol]
      #
      # @api public
      attr_reader :operation

      # The absolute path within the input that this suggestion targets
      #
      # @return [Dry::Schema::Path]
      #
      # @api public
      attr_reader :path

      # The replacement value (only for :replace operation)
      #
      # @return [Object]
      #
      # @api public
      attr_reader :value

      # The error message this suggestion was attached to
      #
      # @return [Message, nil]
      #
      # @api public
      attr_reader :error

      # Coerce a spec into a Suggestion
      #
      # @param [Suggestion, Hash] spec
      #
      # @return [Suggestion]
      #
      # @api private
      def self.[](spec)
        case spec
        when self
          spec
        when ::Hash
          new(**spec.transform_keys(&:to_sym))
        else
          raise ::ArgumentError, "+spec+ must be either a Hash or a #{name} (#{spec.inspect} given)"
        end
      end

      # Build a suggestion that removes the value under the given path
      #
      # @return [Suggestion]
      #
      # @api public
      def self.delete(path)
        new(operation: :delete, path: path)
      end

      # Build a suggestion that replaces the value under the given path
      #
      # @return [Suggestion]
      #
      # @api public
      def self.replace(path, value)
        new(operation: :replace, path: path, value: value)
      end

      # @api private
      def initialize(operation:, path:, value: Undefined, error: nil)
        unless OPERATIONS.include?(operation)
          raise ::ArgumentError,
                "+operation+ must be one of #{OPERATIONS.inspect} (#{operation.inspect} given)"
        end

        @operation = operation
        @path = Schema::Path[path]
        @value = value
        @error = error
      end

      # Return a copy of this suggestion bound to the given error message
      #
      # @return [Suggestion]
      #
      # @api private
      def with_error(error)
        self.class.new(operation: operation, path: path, value: value, error: error)
      end

      # @api public
      def delete?
        operation.equal?(:delete)
      end

      # @api public
      def replace?
        operation.equal?(:replace)
      end

      # Check if this suggestion cannot be merged with another one
      #
      # Two suggestions conflict when:
      # - they target the exact same path with different operations or values
      # - one targets a path that is a parent or a child of the other's path
      # - they target different indices of the same array and at least one of
      #   them deletes an element (deleting shifts the remaining indices)
      #
      # Identical suggestions never conflict - they are treated as one.
      #
      # @return [Boolean]
      #
      # @api public
      def conflicts_with?(other)
        return false if self == other
        return true if path.include?(other.path) || other.path.include?(path)

        array_index_conflict?(other)
      end

      # Apply this suggestion to a (mutable) input hash
      #
      # Missing intermediate nodes are tolerated - the input is left untouched
      # when the target cannot be reached.
      #
      # @param [Hash] input
      #
      # @return [Hash] the same input, modified in place
      #
      # @api private
      def apply(input)
        *parent_keys, last_key = path.keys
        target = parent_keys.reduce(input) do |node, key|
          node.is_a?(::Hash) || node.is_a?(::Array) ? node[key] : nil
        end

        case operation
        when :delete
          case target
          when ::Hash then target.delete(last_key)
          when ::Array then target.delete_at(last_key) if last_key.is_a?(::Integer)
          end
        when :replace
          target[last_key] = value if target.is_a?(::Hash) || target.is_a?(::Array)
        end

        input
      end

      # Return a nice string representation
      #
      # @return [String]
      #
      # @api public
      def inspect
        attrs = +"operation=#{operation.inspect} path=#{path.keys.inspect}"
        attrs << " value=#{value.inspect}" if replace?
        "#<#{self.class} #{attrs}>"
      end

      # Deep-dup a hash/array structure so suggestions can be applied without
      # mutating the original input
      #
      # @api private
      def self.deep_dup(node)
        case node
        when ::Hash
          node.each_with_object({}) { |(key, value), copy| copy[key] = deep_dup(value) }
        when ::Array
          node.map { |value| deep_dup(value) }
        else
          node
        end
      end

      private

      # @api private
      def array_index_conflict?(other)
        keys = path.keys
        other_keys = other.path.keys
        common = keys.zip(other_keys).take_while { |own, foreign| own == foreign }.size

        return false unless keys[common].is_a?(::Integer) && other_keys[common].is_a?(::Integer)

        delete? || other.delete?
      end
    end
  end
end
