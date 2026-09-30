# frozen_string_literal: true

require "dry/container/item"

module Dry
  class Container
    class Item
      # Resolves the item once per scope and caches the value on that scope
      #
      # @api public
      class Scoped < Item
        # @param [Mixed] item
        # @param [Hash] options
        #
        # @raise [Dry::Container::Error]
        #
        # @return [Dry::Container::Item::Scoped]
        def initialize(item, options = {})
          super

          unless callable?
            raise ::ArgumentError,
                  "scope lifetime is only supported for a block or a proc"
          end
        end

        # @return [Mixed]
        def call
          frame = ScopeState.current_frame
          scope = frame&.scope

          unless scope
            raise ::Dry::Container::NoActiveScopeError,
                  %(cannot resolve scope-level item outside of an active scope)
          end

          scope.resolve_cached(frame.key) { item.call }
        end
      end
    end
  end
end
