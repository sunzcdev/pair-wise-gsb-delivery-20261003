# frozen_string_literal: true

module Dry
  module Validation
    # Message message
    #
    # @api public
    class Message < Schema::Message
      include ::Dry::Equalizer(:text, :path, :meta, :warning)

      # The error message text
      #
      # @return [String] text
      #
      # @api public
      attr_reader :text

      # The path to the value with the error
      #
      # @return [Array<Symbol, Integer>]
      #
      # @api public
      attr_reader :path

      # Optional hash with meta-data
      #
      # @return [Hash]
      #
      # @api public
      attr_reader :meta

      # Whether this is a warning rather than a failure
      #
      # Warnings never make a result fail, they are collected separately
      # in {Result#warnings}
      #
      # @return [Boolean]
      #
      # @api public
      attr_reader :warning

      # A localized message type
      #
      # Localized messsages can be translated to other languages at run-time
      #
      # @api public
      class Localized < Message
        # Evaluate message text using provided locale
        #
        # @example
        #   result.errors[:email].evaluate(locale: :en, full: true)
        #   # "email is invalid"
        #
        # @param [Hash] opts
        # @option opts [Symbol] :locale Which locale to use
        # @option opts [Boolean] :full Whether message text should include the key name
        #
        # @api public
        def evaluate(**opts)
          evaluated_text, rest = text.(**opts)
          Message.new(evaluated_text, path: path, meta: rest.merge(meta), warning: warning?)
        end
      end

      # Build an error
      #
      # @return [Message, Message::Localized]
      #
      # @api private
      def self.[](text, path, meta, warning: false)
        klass = text.respond_to?(:call) ? Localized : Message
        klass.new(text, path: path, meta: meta, warning: warning)
      end

      # Initialize a new error object
      #
      # @api private
      # rubocop: disable Lint/MissingSuper
      def initialize(text, path:, meta: EMPTY_HASH, warning: false)
        @text = text
        @path = Array(path)
        @meta = meta
        @warning = warning
      end
      # rubocop: enable Lint/MissingSuper

      # Check if this is a warning rather than a failure
      #
      # @return [Boolean]
      #
      # @api public
      def warning?
        @warning
      end

      # Check if this is a base error not associated with any key
      #
      # @return [Boolean]
      #
      # @api public
      def base?
        @base ||= path.compact.empty?
      end

      # Dump error to a string
      #
      # @return [String]
      #
      # @api public
      def to_s
        text
      end
    end
  end
end
