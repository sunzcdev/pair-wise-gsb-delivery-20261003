# frozen_string_literal: true

module JMESPath
  class QuerySet
    # A single linear step produced when dot/pipe chains are flattened.
    #
    # @!attribute node the AST node evaluated at this position
    # @!attribute marker how this step was joined to the preceding one
    #   (`:root` for the first step, `:dot` for `.`, `:pipe` for `|`)
    Step = Struct.new(:node, :marker) do
      def key
        # The marker is part of the key: `a.b` and `a | b` are prefixes of
        # different provenance and must not be shared.
        [marker, AST.fingerprint(node)]
      end
    end

    # Flattens an expression AST into a linear list of {Step}s.
    #
    # Only {Nodes::Subexpression}/{Nodes::Pipe} nodes are linear, because at
    # evaluation they are pure function composition (`right(left(value)`).
    # Every other node type stays intact and is treated as an opaque step
    # whose *internal* children are fused separately (by the planner).
    module Normalizer
      module_function

      def to_steps(node, marker = :root)
        if node.is_a?(Nodes::Subexpression)
          # left is evaluated first, then right is applied to its result;
          # the boundary marker belongs to the `right` step.
          to_steps(node.left, marker).concat(to_steps(node.right, node.source_marker))
        else
          [Step.new(node, marker)]
        end
      end
    end
  end
end
