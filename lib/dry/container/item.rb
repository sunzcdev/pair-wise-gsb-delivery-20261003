# frozen_string_literal: true

module Dry
  class Container
    # Base class to abstract Memoizable and Callable implementations
    #
    # @api abstract
    #
    class Item
      # Lifecycles supported by registered items, from shortest to longest lived
      #
      # @api public
      LIFECYCLES = %i[resolution scope application].freeze

      # @return [Mixed] the item to be solved later
      attr_reader :item

      # @return [Hash] the options to memoize, call or no.
      attr_reader :options

      # @return [Symbol] one of {LIFECYCLES}
      attr_reader :lifecycle

      # @api abstract
      def initialize(item, options = {})
        @item = item
        @options = {
          call: item.is_a?(::Proc) && item.parameters.empty?
        }.merge(options)

        @lifecycle = resolve_lifecycle
      end

      # @api abstract
      def call
        raise NotImplementedError
      end

      # Resolve the item within the given resolution context
      #
      # The context tracks the active scope and the construction chain, which
      # allows lifecycle violations to be reported while objects are built.
      #
      # @param [Dry::Container::ResolutionContext, nil] context
      # @param [String, nil] key the key the item is registered under
      #
      # @return [Mixed]
      #
      # @api private
      def resolve(context = nil, key = nil)
        call
      end

      # @private
      def value?
        !callable?
      end

      # @private
      def callable?
        options[:call]
      end

      # Build a new item with transformation applied
      #
      # @private
      def map(func)
        if callable?
          self.class.new(-> { func.(item.call) }, options)
        else
          self.class.new(func.(item), options)
        end
      end

      private

      # @private
      def resolve_lifecycle
        lifecycle = options[:lifecycle]

        if lifecycle.nil?
          options[:memoize] ? :application : :resolution
        else
          lifecycle = lifecycle.to_sym

          unless LIFECYCLES.include?(lifecycle)
            raise ScopeError,
                  "unknown lifecycle #{lifecycle.inspect}, " \
                  "must be one of #{LIFECYCLES.map(&:inspect).join(", ")}"
          end

          lifecycle
        end
      end
    end
  end
end
