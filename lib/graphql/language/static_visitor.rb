# frozen_string_literal: true
module GraphQL
  module Language
    # Like `GraphQL::Language::Visitor` except it doesn't support
    # making changes to the document -- only visiting it as-is.
    class StaticVisitor
      def initialize(document)
        @document = document
      end

      # Visit `document` and all children
      # @return [void]
      def visit
        # `@document` may be any kind of node:
        visit_method = @document.visit_method
        result = public_send(visit_method, @document, nil)
        @result = if result.is_a?(Array)
          result.first
        else
          # The node wasn't modified
          @document
        end
      end

      def on_document_children(document_node)
        document_node.children.each do |child_node|
          visit_child_node(child_node, document_node)
        end
      end

      def on_field_children(new_node)
        new_node.arguments.each do |arg_node| # rubocop:disable Development/ContextIsPassedCop
          visit_child_node(arg_node, new_node)
        end
        visit_directives(new_node)
        visit_selections(new_node)
      end

      def visit_directives(new_node)
        new_node.directives.each do |dir_node|
          visit_child_node(dir_node, new_node)
        end
      end

      def visit_selections(new_node)
        new_node.selections.each do |selection|
          case selection
          when GraphQL::Language::Nodes::Field
            visit_child_node(selection, new_node)
          when GraphQL::Language::Nodes::InlineFragment
            visit_child_node(selection, new_node)
          when GraphQL::Language::Nodes::FragmentSpread
            visit_child_node(selection, new_node)
          else
            raise ArgumentError, "Invariant: unexpected field selection #{selection.class} (#{selection.inspect})"
          end
        end
      end

      def on_fragment_definition_children(new_node)
        visit_directives(new_node)
        visit_selections(new_node)
      end

      alias :on_inline_fragment_children :on_fragment_definition_children

      def on_operation_definition_children(new_node)
        new_node.variables.each do |arg_node|
          visit_child_node(arg_node, new_node)
        end
        visit_directives(new_node)
        visit_selections(new_node)
      end

      def on_argument_children(new_node)
        new_node.children.each do |value_node|
          case value_node
          when Language::Nodes::VariableIdentifier
            visit_child_node(value_node, new_node)
          when Language::Nodes::InputObject
            visit_child_node(value_node, new_node)
          when Language::Nodes::Enum
            visit_child_node(value_node, new_node)
          when Language::Nodes::NullValue
            visit_child_node(value_node, new_node)
          else
            raise ArgumentError, "Invariant: unexpected argument value node #{value_node.class} (#{value_node.inspect})"
          end
        end
      end

      # Dispatch a single child node's visit hook.
      #
      # Every check performed against the document (one hook entry per AST node,
      # including all of its included rule callbacks) is reached through this
      # method, so it's the place where a unit of validation work is accounted
      # for. Subclasses may wrap it (for example to pause when a work budget is
      # exhausted), but the order and the hook being called stay identical.
      def visit_child_node(child_node, parent_node)
        public_send(child_node.visit_method, child_node, parent_node)
      end

      # rubocop:disable Development/NoEvalCop This eval takes static inputs at load-time

      # We don't use `alias` here because it breaks `super`
      def self.make_visit_methods(ast_node_class)
        node_method = ast_node_class.visit_method
        children_of_type = ast_node_class.children_of_type
        child_visit_method = :"#{node_method}_children"

        class_eval(<<-RUBY, __FILE__, __LINE__ + 1)
          # The default implementation for visiting an AST node.
          # It doesn't _do_ anything, but it continues to visiting the node's children.
          # To customize this hook, override one of its make_visit_methods (or the base method?)
          # in your subclasses.
          #
          # @param node [GraphQL::Language::Nodes::AbstractNode] the node being visited
          # @param parent [GraphQL::Language::Nodes::AbstractNode, nil] the previously-visited node, or `nil` if this is the root node.
          # @return [void]
          def #{node_method}(node, parent)
            #{
              if method_defined?(child_visit_method)
                "#{child_visit_method}(node)"
              elsif children_of_type
                children_of_type.map do |child_accessor, child_class|
                  "node.#{child_accessor}.each do |child_node|
                    visit_child_node(child_node, node)
                  end"
                end.join("\n")
              else
                ""
              end
            }
          end
        RUBY
      end

      [
        Language::Nodes::Argument,
        Language::Nodes::Directive,
        Language::Nodes::DirectiveDefinition,
        Language::Nodes::DirectiveLocation,
        Language::Nodes::Document,
        Language::Nodes::Enum,
        Language::Nodes::EnumTypeDefinition,
        Language::Nodes::EnumTypeExtension,
        Language::Nodes::EnumValueDefinition,
        Language::Nodes::Field,
        Language::Nodes::FieldDefinition,
        Language::Nodes::FragmentDefinition,
        Language::Nodes::FragmentSpread,
        Language::Nodes::InlineFragment,
        Language::Nodes::InputObject,
        Language::Nodes::InputObjectTypeDefinition,
        Language::Nodes::InputObjectTypeExtension,
        Language::Nodes::InputValueDefinition,
        Language::Nodes::InterfaceTypeDefinition,
        Language::Nodes::InterfaceTypeExtension,
        Language::Nodes::ListType,
        Language::Nodes::NonNullType,
        Language::Nodes::NullValue,
        Language::Nodes::ObjectTypeDefinition,
        Language::Nodes::ObjectTypeExtension,
        Language::Nodes::OperationDefinition,
        Language::Nodes::ScalarTypeDefinition,
        Language::Nodes::ScalarTypeExtension,
        Language::Nodes::SchemaDefinition,
        Language::Nodes::SchemaExtension,
        Language::Nodes::TypeName,
        Language::Nodes::UnionTypeDefinition,
        Language::Nodes::UnionTypeExtension,
        Language::Nodes::VariableDefinition,
        Language::Nodes::VariableIdentifier,
      ].each do |ast_node_class|
        make_visit_methods(ast_node_class)
      end

      # rubocop:disable Development/NoEvalCop
    end
  end
end
