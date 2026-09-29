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
      #   @example attach structured fix candidates via the :suggestions key
      #     failure({text: :invalid, suggestions: [{operation: :delete, path: [:legacy]}]})
      #
      # @overload failure(message, tokens, suggestions)
      #   Attach structured fix candidates that could resolve this failure.
      #   Each entry is either a Suggestion or a Hash like
      #   `{operation: :replace, path: [:status], value: "active"}` or
      #   `{operation: :delete, path: [:legacy_field]}`
      #   @param message [String, Symbol] The message text or identifier
      #   @param tokens [Hash] The message tokens
      #   @param suggestions [Array<Suggestion, Hash>] The fix candidates
      #   @example
      #     failure("is invalid", {}, [{operation: :delete, path: [:legacy]}])
      #
      # @see Evaluator#key
      # @see Evaluator#base
      #
      # @api public
      def failure(message, tokens = EMPTY_HASH, suggestions = EMPTY_ARRAY)
        opts << {message: message, tokens: tokens, path: path, suggestions: suggestions}
        self
      end

      # @api private
      def empty?
        opts.empty?
      end
    end
  end
end
