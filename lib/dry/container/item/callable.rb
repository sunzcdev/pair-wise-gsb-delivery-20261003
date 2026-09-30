# frozen_string_literal: true

require "dry/container/item"

module Dry
  class Container
    class Item
      # Callable class to returns a item call
      #
      # @api public
      #
      class Callable < Item
        # Returns the result of item call or item
        #
        # @return [Mixed]
        def call(**dependencies)
          callable? ? item.call(**dependencies) : item
        end
      end
    end
  end
end
