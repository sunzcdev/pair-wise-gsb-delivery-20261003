module Dentaku
  module AST
    class Node
      # reserved context key (double-underscore prefix, like
      # __evaluation_mode) that switches dependencies() into static mode:
      # guards are never evaluated and every branch is reported
      STATIC_MODE_KEY = "__static_dependencies".freeze
      STATIC_CONTEXT = { STATIC_MODE_KEY => true }.freeze

      # Range of character offsets (exclusive end) into the original formula
      # string that this node was parsed from; nil for synthetic nodes
      attr_writer :location

      def location
        @location
      end

      # evaluates this node, annotating any Dentaku error with the source
      # locations of the whole chain of enclosing expressions
      def value(context = {})
        evaluate(context)
      rescue Dentaku::Error => e
        e.record_trace(self)
        raise
      end

      def self.precedence
        0
      end

      def self.arity
        nil
      end

      def self.resolve_class(*)
        self
      end

      def dependencies(context = {})
        []
      end

      # whether this subtree may be evaluated during dependency resolution
      # without running volatile user code; a property of the parsed AST,
      # computed once and memoized
      def pure?
        return @pure if defined?(@pure)

        @pure = compute_pure?
      end

      def type
        nil
      end

      def name
        self.class.name.to_s.split("::").last.upcase
      end

      private

      def static_mode?(context)
        context[STATIC_MODE_KEY] == true
      end

      def compute_pure?
        true
      end
    end
  end
end
