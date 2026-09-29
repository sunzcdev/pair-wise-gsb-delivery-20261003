# frozen_string_literal: true
module JMESPath
  # @api private
  module Nodes
    class Current < Node
      def visit(value)
        value
      end

      def streamable?
        true
      end

      def streams_array?
        true
      end

      def stream_each(values)
        values
      end
    end
  end
end
