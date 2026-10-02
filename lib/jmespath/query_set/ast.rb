# frozen_string_literal: true

module JMESPath
  class QuerySet
    # Structural, semantic analysis of *raw* (pre-`#optimize`) JMESPath AST
    # nodes. Everything here works on AST objects, never on expression
    # strings: two nodes are considered equal only when they are built from
    # the same node types and operands.
    #
    # Notable distinctions that are preserved:
    #
    # * a {Nodes::Pipe} boundary is tagged `:pipe`, a dotted chain `:dot`;
    # * an expression reference (`&foo`) is an opaque boundary, the AST it
    #   carries is only shared with *the very same reference*, never fused
    #   with an ordinary sub-prefix;
    # * a filter projection carries its condition, so different predicates
    #   never share the filter step (they still share everything before it).
    module AST
      module_function

      # @return [Symbol] coarse shape used to dispatch evaluation/fusion
      def kind(node)
        case node
        when Nodes::Pipe then :subexpression
        when Nodes::Subexpression then :subexpression
        when Nodes::ArrayProjection then :array_projection
        when Nodes::ObjectProjection then :object_projection
        when Nodes::Flatten then :flatten
        when Nodes::Condition then :condition
        when Nodes::MultiSelectHash then :multi_select_hash
        when Nodes::MultiSelectList then :multi_select_list
        when Nodes::Function then :function
        when Nodes::Expression then :expression
        when Nodes::And then :and
        when Nodes::Or then :or
        when Nodes::Not then :not
        when Nodes::Comparator then :comparator
        else :leaf
        end
      end

      # Internal child nodes of `node` in a canonical order. "Internal" means
      # children that are evaluated with the *same input* as `node` itself
      # (targets, operands, arguments, tests). Projection continuations are
      # deliberately excluded: they run per projected element and are handled
      # separately.
      def internals(node)
        case kind(node)
        when :subexpression then [node.left, node.right]
        when :array_projection, :object_projection then [node.target]
        when :flatten then [node.child]
        when :condition then [node.test]
        when :multi_select_hash then node.kv_pairs.map(&:value)
        when :multi_select_list, :function then node.children.dup
        when :and, :or, :comparator then [node.left, node.right]
        when :not then [node.expression]
        when :expression then [] # opaque: carried body is never fused
        else []
        end
      end

      def function_name(node)
        node.class.name.sub(/.*::/, ''.freeze)
      end

      # Recursive structural fingerprint of a complete node tree. Memoized on
      # the node; parsed ASTs are immutable, so caching is safe.
      def fingerprint(node)
        node.instance_variable_get(:@qs_fingerprint) ||
          node.instance_variable_set(:@qs_fingerprint, build_fingerprint(node))
      end

      def build_fingerprint(node)
        fp = [node.class.name]
        case kind(node)
        when :subexpression
          fp.concat([:sub, node.source_marker, fingerprint(node.left), fingerprint(node.right)])
        when :array_projection, :object_projection
          fp.concat([kind(node), fingerprint(node.target), fingerprint(node.projection)])
        when :flatten then fp.concat([:flatten, fingerprint(node.child)])
        when :condition then fp.concat([:cond, fingerprint(node.test), fingerprint(node.child)])
        when :multi_select_hash
          fp << :msh
          fp.concat(node.kv_pairs.map { |p| [p.key, fingerprint(p.value)] })
        when :multi_select_list
          fp.concat([:msl, node.children.map { |c| fingerprint(c) }])
        when :function
          fp.concat([:fn, function_name(node), node.children.map { |c| fingerprint(c) }])
        when :and, :or then fp.concat([kind(node), fingerprint(node.left), fingerprint(node.right)])
        when :comparator
          fp.concat([:cmp, node.class.name, fingerprint(node.left), fingerprint(node.right)])
        when :not then fp.concat([:not, fingerprint(node.expression)])
        when :expression then fp.concat([:expref, fingerprint(node.expression)])
        when :leaf
          node.instance_variables.each do |ivar|
            v = node.instance_variable_get(ivar)
            next if v.is_a?(Nodes::Node)

            fp << ivar << v
          end
        end
        fp
      end
    end
  end
end
