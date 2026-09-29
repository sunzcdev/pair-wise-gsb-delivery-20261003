# frozen_string_literal: true

require "dry/initializer"
require "dry/validation/constants"

module Dry
  module Validation
    # A sub-contract mounted at a specific path of a parent contract
    #
    # Mounts apply another contract to a nested value (a single hash or an
    # array of hashes) and merge the outcome back into the parent result:
    #
    # * the sub-contract's output replaces the value at the mount path
    # * errors are re-pathed under the mount path, base errors are attached
    #   to the mount path itself and error meta data is preserved
    # * context is passed to the sub-contract according to the mount
    #   declaration, so mounted contracts never share mutable state
    #
    # @see ClassInterface#mount
    #
    # @api public
    class Mount
      include ::Dry::Equalizer(:path, :contract, :each, :context, inspect: false)

      extend ::Dry::Initializer

      # @!attribute [r] path
      #   @return [Array<Symbol>] Path at which the sub-contract is mounted
      #   @api public
      option :path

      # @!attribute [r] contract
      #   @return [Contract] The mounted sub-contract
      #   @api public
      option :contract

      # @!attribute [r] each
      #   @return [Boolean] Whether the mounted value is an array of hashes
      #   @api public
      option :each, default: -> { false }

      # @!attribute [r] context
      #   @return [Boolean, Array<Symbol>, Hash] Context passing declaration
      #   @api public
      option :context, default: -> { false }

      # Apply the mounted contract and merge its output and errors into the
      # parent result
      #
      # @param [Result] result The parent contract's result
      #
      # @return [Result]
      #
      # @api private
      def call(result)
        return result if result.schema_error?(path)
        return result unless result.key?(path)

        if each
          call_each(result, result.values[path])
        else
          call_one(result, result.values[path])
        end

        result
      end

      # Return a nice string representation
      #
      # @return [String]
      #
      # @api public
      def inspect
        %(#<#{self.class} path=#{path.inspect} contract=#{contract.class} each=#{each}>)
      end

      private

      # @api private
      def call_one(result, value)
        return unless value.is_a?(::Hash)

        sub_result = contract.(value, context_for(result))

        result.set_value(path, sub_result.to_h)
        import_errors(result, sub_result, path)
      end

      # @api private
      def call_each(result, value)
        return unless value.is_a?(::Array)

        output = value.each_with_index.map do |item, index|
          item_path = [*path, index]

          if item.is_a?(::Hash) && !result.schema_error?(item_path)
            sub_result = contract.(item, context_for(result))
            import_errors(result, sub_result, item_path)
            sub_result.to_h
          else
            item
          end
        end

        result.set_value(path, output)
      end

      # @api private
      def import_errors(result, sub_result, target_path)
        sub_result.errors.each do |message|
          message_path = Schema::Path[message.path].to_a.compact
          full_path = message_path.empty? ? target_path : target_path + message_path
          meta = message.respond_to?(:meta) ? message.meta : EMPTY_HASH

          result.add_error(Message[message.text, full_path, meta])
        end
      end

      # @api private
      def context_for(result)
        case context
        when true
          result.context.each.to_h
        when ::Array
          result.context.each.to_h.slice(*context)
        when ::Hash
          context
        else
          EMPTY_HASH
        end
      end
    end
  end
end
