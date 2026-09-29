# frozen_string_literal: true
module JMESPath
  # @api private
  module Nodes
    class Node
      def visit(value)
      end

      def optimize
        self
      end

      def chains_with?(_other)
        false
      end

      private

      # Records a diagnostic entry and returns the MISSING sentinel.
      # Only called when a DiagnosticContext is active.
      def record_missing(path, reason = 'missing_key')
        DiagnosticContext.current.record(path, reason)
        MISSING
      end

      def append_diagnostic_path(path, key)
        if key.is_a?(Integer)
          "#{path}[#{key}]"
        elsif path.empty?
          key.to_s
        else
          "#{path}.#{key}"
        end
      end
    end

    require  'jmespath/nodes/subexpression'
    require  'jmespath/nodes/and'
    require  'jmespath/nodes/comparator'
    require  'jmespath/nodes/comparator'
    require  'jmespath/nodes/condition'
    require  'jmespath/nodes/current'
    require  'jmespath/nodes/expression'
    require  'jmespath/nodes/field'
    require  'jmespath/nodes/flatten'
    require  'jmespath/nodes/function'
    require  'jmespath/nodes/index'
    require  'jmespath/nodes/literal'
    require  'jmespath/nodes/multi_select_hash'
    require  'jmespath/nodes/multi_select_list'
    require  'jmespath/nodes/not'
    require  'jmespath/nodes/or'
    require  'jmespath/nodes/pipe'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/projection'
    require  'jmespath/nodes/slice'
  end
end
