# frozen_string_literal: true

module Rack
  class Cors
    class Evaluation
      def initialize
        @results = {}
      end

      def evaluate(condition, *args)
        key = condition.object_id
        return @results[key] if @results.key?(key)

        @results[key] = condition.call(*args)
      end
    end
  end
end
