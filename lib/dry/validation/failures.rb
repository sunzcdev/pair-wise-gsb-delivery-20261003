# frozen_string_literal: true

require "dry/schema/path"
require "dry/validation/constants"

module Dry
  module Validation
    # Failure accumulator object
    #
    # @api public
    class Failures
      # The path for messages accumulated by failures object
      #
      # @return [Dry::Schema::Path]
      #
      # @api private
      attr_reader :path

      # Options for messages
      #
      # These options are used by MessageResolver
      #
      # @return [Hash]
      #
      # @api private
      attr_reader :opts

      # @api private
      def initialize(path = ROOT_PATH)
        @path = ::Dry::Schema::Path[path]
        @opts = EMPTY_ARRAY.dup
      end

      # Set failure
      #
      # @overload failure(message)
      #   Set message text explicitly
      #   @param message [String] The message text
      #   @example
      #     failure('this failed')
      #
      # @overload failure(id)
      #   Use message identifier (needs localized messages setup)
      #   @param id [Symbol] The message id
      #   @example
      #     failure(:taken)
      #
      # @overload failure(meta_hash)
      #   Use meta_hash[:text] as a message (either explicitely or as an identifier),
      #   setting the rest of the hash as error meta attribute
      #   @param meta [Hash] The hash containing the message as value for the :text key
      #   @example
      #     failure({text: :invalid, key: value})
      #
      # @see Evaluator#key
      # @see Evaluator#base
      #
      # @api public
      def failure(message, tokens = EMPTY_HASH)
        opts << {message: message, tokens: tokens, path: path}
        self
      end

      # Set warning
      #
      # A warning is a non-blocking diagnostic: it shares the same path and
      # message resolution as a failure, but it never makes the result fail.
      #
      # @overload warning(message)
      #   Set message text explicitly
      #   @param message [String] The message text
      #   @example
      #     warning('this is deprecated')
      #
      # @overload warning(id)
      #   Use message identifier (needs localized messages setup)
      #   @param id [Symbol] The message id
      #   @example
      #     warning(:legacy_format)
      #
      # @overload warning(meta_hash)
      #   Use meta_hash[:text] as a message (either explicitely or as an identifier),
      #   setting the rest of the hash as message meta attribute
      #   @param meta [Hash] The hash containing the message as value for the :text key
      #   @example
      #     warning({text: :deprecated, code: 102})
      #
      # @see Failures#failure
      #
      # @api public
      def warning(message, tokens = EMPTY_HASH)
        opts << {message: message, tokens: tokens, path: path, warning: true}
        self
      end

      # Return only failure-level options
      #
      # @return [Array<Hash>]
      #
      # @api private
      def failures
        opts.reject { |opt| opt[:warning] }
      end

      # Return only warning-level options
      #
      # @return [Array<Hash>]
      #
      # @api private
      def warning_messages
        opts.select { |opt| opt[:warning] }
      end

      # @api private
      def empty?
        failures.empty?
      end
    end
  end
end
