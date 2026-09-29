# frozen_string_literal: true

require "dry/validation/constants"

module Dry
  module Validation
    # A contract mounted at a path within another contract
    #
    # Mounted contracts are applied to the (nested) input found under their
    # mount path when the parent contract is called. Their output is written
    # back to the parent result and their errors are moved to the mount path.
    #
    # @see Contract::ClassInterface#mount
    #
    # @api private
    class MountedContract
      # @!attribute [r] contract
      #   @return [Contract] The mounted contract instance
      #   @api private
      attr_reader :contract

      # @!attribute [r] path
      #   @return [Array<Symbol, Integer>] The mount path within the parent input
      #   @api private
      attr_reader :path

      # @!attribute [r] context_option
      #   @return [true, false, Hash] Context passing option
      #   @api private
      attr_reader :context_option

      # @api private
      def initialize(contract, path:, each:, context:)
        @contract = contract.is_a?(::Class) ? contract.new : contract
        @path = Schema::Path[path].to_a
        @each = each
        @context_option = context
      end

      # @api private
      def each?
        @each
      end

      # Apply the mounted contract and merge its output and errors into the
      # parent result
      #
      # @param [Result] result The parent contract's result
      #
      # @return [Result] The parent result
      #
      # @api private
      def call(result)
        targets(result).each do |target_path, input|
          next if result.error?(target_path)

          mounted_result = contract.(input, context_for(result))

          write_value(result.values.data, target_path, mounted_result.to_h)

          mounted_result.errors.each do |error|
            result.add_error(move_error(error, target_path))
          end
        end

        result
      end

      private

      # @api private
      def targets(result)
        value = result.values[path]

        if each?
          return EMPTY_ARRAY unless value.is_a?(::Array)

          value.each_with_index.filter_map do |element, index|
            [path + [index], element] if element.is_a?(::Hash)
          end
        elsif value.is_a?(::Hash)
          [[path, value]]
        else
          EMPTY_ARRAY
        end
      end

      # Build the context for the mounted contract
      #
      # A new hash is used for every application so that mount points (and
      # individual array elements) can never write into each other's state
      #
      # @api private
      def context_for(result)
        case context_option
        when true
          result.context.each.to_h
        when ::Hash
          context_option
        else
          EMPTY_HASH
        end
      end

      # @api private
      def write_value(data, target_path, value)
        *keys, last = target_path
        keys.reduce(data) { |obj, key| obj[key] }[last] = value
      end

      # @api private
      def move_error(error, target_path)
        error_path = error.path.compact
        path = error_path.empty? ? target_path : target_path + error_path

        Message[error.text, path, error.meta]
      end
    end
  end
end
