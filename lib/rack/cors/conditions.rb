# frozen_string_literal: true

module Rack
  class Cors
    # Memoizes dynamic allow/resource conditions for the lifetime of one
    # request. A Proc that is reached by more than one rule is still invoked
    # exactly once.
    class Conditions
      def initialize
        @results = {}
      end

      def evaluate(condition, *args)
        @results.fetch([condition, args]) do
          @results[[condition, args]] = condition.call(*args)
        end
      end
    end
  end
end
