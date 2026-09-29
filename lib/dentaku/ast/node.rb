require_relative '../exceptions'

module Dentaku
  module AST
    class Node
      # reserved context key (double-underscore prefix, like
      # __evaluation_mode) that switches dependencies() into static mode:
      # guards are never evaluated and every branch is reported
      STATIC_MODE_KEY = "__static_dependencies".freeze
      STATIC_CONTEXT = { STATIC_MODE_KEY => true }.freeze

      # Dentaku::SourceRange of this node in the original expression,
      # filled in by the parser
      attr_accessor :source_range

      # Wraps #value so that any error raised while evaluating a node is
      # annotated with that node's source range as it propagates outward.
      # Prepended to every subclass via .inherited.
      module EvaluationGuard
        def value(context = {})
          super
        rescue ::StandardError => e
          Dentaku::Error.attach_source(e, self)
          raise
        end
      end

      def self.inherited(subclass)
        super
        subclass.prepend(EvaluationGuard)
      end

      # whether this node represents a function call site; function calls
      # are recorded on an error's call_trace as it propagates
      def call_site?
        false
      end

      # yields this node and every descendant, so tools can locate the
      # source range of any subexpression
      def each_node(&block)
        return enum_for(:each_node) unless block_given?

        yield self

        instance_variables.each do |ivar|
          child = instance_variable_get(ivar)
          case child
          when Node
            child.each_node(&block)
          when Array
            child.each { |element| element.each_node(&block) if element.is_a?(Node) }
          end
        end
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
