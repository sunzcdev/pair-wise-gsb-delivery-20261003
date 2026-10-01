# frozen_string_literal: true
module GraphQL
  module Language
    # Merges several parsed SDL documents -- each tagged with a source
    # identifier -- into a single {Nodes::Document}.
    #
    # Concatenating SDL strings from different modules defers duplicate
    # definitions and conflicting extensions until schema build time (or
    # later). This merge validates as it goes instead:
    #
    # - A type may be defined only once across all documents.
    # - Extensions are resolved against definitions from _any_ merged
    #   document, so cross-document (and mutually recursive) references
    #   are fine. Extensions are applied only after every document has
    #   been read.
    # - Fields (and input fields / arguments) that are declared more than
    #   once must agree on their type and on argument default values.
    # - Root operation types from `schema` / `extend schema` must agree.
    # - Directive definitions that appear in several documents must be
    #   identical; non-repeatable directive applications must not
    #   conflict.
    #
    # The input ASTs are never modified; the merged document is built
    # from copies.
    #
    # @example Merging two parsed documents
    #   doc_a = GraphQL.parse(File.read("a.graphql"))
    #   doc_b = GraphQL.parse(File.read("b.graphql"))
    #   merged = GraphQL::Language::DocumentMerge.merge("a.graphql" => doc_a, "b.graphql" => doc_b)
    #   GraphQL::Schema.from_definition(merged.to_query_string)
    #
    module DocumentMerge
      # Raised when documents can't be merged. The message identifies
      # both conflicting declarations by source and position.
      class MergeError < GraphQL::Error; end

      TYPE_DEFINITION_NODES = [
        Nodes::ObjectTypeDefinition,
        Nodes::InterfaceTypeDefinition,
        Nodes::UnionTypeDefinition,
        Nodes::EnumTypeDefinition,
        Nodes::InputObjectTypeDefinition,
        Nodes::ScalarTypeDefinition,
      ].freeze

      EXTENSION_TO_DEFINITION = {
        Nodes::ObjectTypeExtension => Nodes::ObjectTypeDefinition,
        Nodes::InterfaceTypeExtension => Nodes::InterfaceTypeDefinition,
        Nodes::UnionTypeExtension => Nodes::UnionTypeDefinition,
        Nodes::EnumTypeExtension => Nodes::EnumTypeDefinition,
        Nodes::InputObjectTypeExtension => Nodes::InputObjectTypeDefinition,
        Nodes::ScalarTypeExtension => Nodes::ScalarTypeDefinition,
      }.freeze

      EXTENSION_NODES = EXTENSION_TO_DEFINITION.keys.freeze

      # @param documents [Hash{Object => GraphQL::Language::Nodes::Document}] parsed documents, keyed by a source identifier used in error messages
      # @return [GraphQL::Language::Nodes::Document] a new document with all definitions and extensions merged
      def self.merge(documents)
        merger = Merger.new
        documents.each_pair do |source, document|
          if !document.is_a?(Nodes::Document)
            raise ArgumentError, "expected #{source.inspect} to be a GraphQL::Language::Nodes::Document, got #{document.class}"
          end
          merger.add(source, document)
        end
        merger.result
      end

      # @api private
      class Merger
        def initialize
          @node_sources = {}.compare_by_identity
          @type_definitions = {} # name => definition node, in order of first appearance
          @directive_definitions = {} # name => definition node
          @schema_nodes = [] # SchemaDefinition / SchemaExtension nodes, in order
          @extensions = [] # type extension nodes, in order
        end

        def add(source, document)
          document.definitions.each do |definition|
            register_source(definition, source)
            case definition
            when *TYPE_DEFINITION_NODES
              if (existing = @type_definitions[definition.name])
                raise MergeError, "Type \"#{definition.name}\" is defined more than once (#{location_of(existing)} and #{location_of(definition)})"
              end
              @type_definitions[definition.name] = definition
            when Nodes::DirectiveDefinition
              if (existing = @directive_definitions[definition.name])
                assert_compatible_directive_definitions(existing, definition)
              else
                @directive_definitions[definition.name] = definition
              end
            when Nodes::SchemaDefinition, Nodes::SchemaExtension
              @schema_nodes << definition
            when *EXTENSION_NODES
              @extensions << definition
            else
              raise MergeError, "Can't merge #{definition.class.name.split("::").last} at #{location_of(definition)}: only type system definitions and extensions are supported"
            end
          end
        end

        def result
          # All material is present now; resolve extensions, which may
          # refer to definitions from any of the merged documents.
          @extensions.each { |extension| apply_extension(extension) }

          definitions = []
          if (schema_definition = merged_schema_definition)
            definitions << schema_definition
          end
          @directive_definitions.each_value { |defn| definitions << defn }
          @type_definitions.each_value { |defn| definitions << defn }
          Nodes::Document.new(definitions: definitions)
        end

        private

        def apply_extension(extension)
          base = @type_definitions[extension.name]
          if base.nil?
            raise MergeError, "Cannot extend type \"#{extension.name}\" (#{location_of(extension)}): no definition for it exists in any merged document"
          end

          expected_class = EXTENSION_TO_DEFINITION[extension.class]
          if !base.is_a?(expected_class)
            raise MergeError, "Cannot extend #{kind_name(base)} \"#{extension.name}\" (#{location_of(base)}) with a #{kind_name(extension)} (#{location_of(extension)})"
          end

          merged = case extension
          when Nodes::ObjectTypeExtension, Nodes::InterfaceTypeExtension
            base.merge(
              interfaces: merge_named_types(base.interfaces, extension.interfaces),
              fields: merge_fields(base.fields, extension.fields, base.name),
              directives: merge_directives(base.directives, extension.directives),
            )
          when Nodes::UnionTypeExtension
            base.merge(
              types: merge_named_types(base.types, extension.types),
              directives: merge_directives(base.directives, extension.directives),
            )
          when Nodes::EnumTypeExtension
            base.merge(
              values: merge_enum_values(base.values, extension.values, base.name),
              directives: merge_directives(base.directives, extension.directives),
            )
          when Nodes::InputObjectTypeExtension
            base.merge(
              fields: merge_arguments(base.fields, extension.fields, "input field", base.name),
              directives: merge_directives(base.directives, extension.directives),
            )
          when Nodes::ScalarTypeExtension
            base.merge(
              directives: merge_directives(base.directives, extension.directives),
            )
          end

          inherit_source(merged, base)
          @type_definitions[extension.name] = merged
        end

        # Fields (or field arguments / input fields) declared in both a
        # definition and an extension must agree on their types; argument
        # defaults must agree when both declarations provide one.
        def merge_fields(base_fields, extension_fields, type_name)
          merged = base_fields.dup
          extension_fields.each do |ext_field|
            if (base_field = merged.find { |f| f.name == ext_field.name })
              if !(base_field.type == ext_field.type)
                raise MergeError, "Conflicting types for field \"#{type_name}.#{base_field.name}\": \"#{type_string(base_field.type)}\" (#{location_of(base_field)}) and \"#{type_string(ext_field.type)}\" (#{location_of(ext_field)})"
              end
              new_arguments = merge_arguments(base_field.arguments, ext_field.arguments, "argument", "#{type_name}.#{base_field.name}")
              new_directives = merge_directives(base_field.directives, ext_field.directives)
              merged[merged.index(base_field)] = inherit_source(
                base_field.merge(arguments: new_arguments, directives: new_directives),
                base_field
              )
            else
              merged << ext_field
            end
          end
          merged
        end

        def merge_arguments(base_arguments, extension_arguments, what, owner_name)
          merged = base_arguments.dup
          extension_arguments.each do |ext_arg|
            if (base_arg = merged.find { |a| a.name == ext_arg.name })
              if !(base_arg.type == ext_arg.type)
                raise MergeError, "Conflicting types for #{what} \"#{owner_name}(#{base_arg.name}:)\": \"#{type_string(base_arg.type)}\" (#{location_of(base_arg)}) and \"#{type_string(ext_arg.type)}\" (#{location_of(ext_arg)})"
              end
              if !base_arg.default_value.nil? && !ext_arg.default_value.nil? && !(base_arg.default_value == ext_arg.default_value)
                raise MergeError, "Conflicting default values for #{what} \"#{owner_name}(#{base_arg.name}:)\": #{default_string(base_arg.default_value)} (#{location_of(base_arg)}) and #{default_string(ext_arg.default_value)} (#{location_of(ext_arg)})"
              end
              default_value = base_arg.default_value.nil? ? ext_arg.default_value : base_arg.default_value
              new_directives = merge_directives(base_arg.directives, ext_arg.directives)
              merged[merged.index(base_arg)] = inherit_source(
                base_arg.merge(default_value: default_value, directives: new_directives),
                base_arg
              )
            else
              merged << ext_arg
            end
          end
          merged
        end

        def merge_enum_values(base_values, extension_values, type_name)
          merged = base_values.dup
          extension_values.each do |ext_value|
            if (base_value = merged.find { |v| v.name == ext_value.name })
              new_directives = merge_directives(base_value.directives, ext_value.directives)
              merged[merged.index(base_value)] = inherit_source(
                base_value.merge(directives: new_directives),
                base_value
              )
            else
              merged << ext_value
            end
          end
          merged
        end

        # Interfaces and union members may be repeated across a
        # definition and its extensions; duplicates are dropped.
        def merge_named_types(base_types, extension_types)
          merged = base_types.dup
          existing_names = {}
          merged.each { |t| existing_names[named_type_name(t)] = true }
          extension_types.each do |t|
            name = named_type_name(t)
            if !existing_names[name]
              existing_names[name] = true
              merged << t
            end
          end
          merged
        end

        # Identical re-applications are dropped; differing applications
        # of the same non-repeatable directive are a conflict.
        def merge_directives(base_directives, extension_directives)
          merged = base_directives.dup
          extension_directives.each do |ext_dir|
            duplicates = merged.select { |d| d.name == ext_dir.name }
            if duplicates.empty?
              merged << ext_dir
            elsif duplicates.any? { |d| d == ext_dir }
              next
            elsif (defn = @directive_definitions[ext_dir.name]) && defn.repeatable
              merged << ext_dir
            else
              raise MergeError, "Conflicting @#{ext_dir.name} directives (#{location_of(duplicates.first)} and #{location_of(ext_dir)})"
            end
          end
          merged
        end

        # A directive may be defined in several documents, but all
        # definitions must be identical.
        def assert_compatible_directive_definitions(existing, new_defn)
          if existing.repeatable != new_defn.repeatable
            raise MergeError, "Conflicting `repeatable` for directive \"@#{existing.name}\" (#{location_of(existing)} and #{location_of(new_defn)})"
          end
          if existing.locations.map(&:name).sort != new_defn.locations.map(&:name).sort
            raise MergeError, "Conflicting locations for directive \"@#{existing.name}\" (#{location_of(existing)} and #{location_of(new_defn)})"
          end
          if existing.arguments.map(&:name).sort != new_defn.arguments.map(&:name).sort
            raise MergeError, "Conflicting arguments for directive \"@#{existing.name}\" (#{location_of(existing)} and #{location_of(new_defn)})"
          end
          merge_arguments(existing.arguments, new_defn.arguments, "argument", "@#{existing.name}")
          nil
        end

        # Root operation types from `schema` / `extend schema` blocks
        # must agree across all documents.
        def merged_schema_definition
          return nil if @schema_nodes.empty?

          roots = {}
          root_nodes = {}
          directives = []
          @schema_nodes.each do |node|
            [:query, :mutation, :subscription].each do |operation|
              value = node.public_send(operation)
              next if value.nil?
              if roots.key?(operation) && roots[operation] != value
                raise MergeError, "Conflicting #{operation} root types: \"#{roots[operation]}\" (#{location_of(root_nodes[operation])}) and \"#{value}\" (#{location_of(node)})"
              end
              roots[operation] = value
              root_nodes[operation] = node
            end
            directives = merge_directives(directives, node.directives)
          end

          inherit_source(
            Nodes::SchemaDefinition.new(
              query: roots[:query],
              mutation: roots[:mutation],
              subscription: roots[:subscription],
              directives: directives,
            ),
            @schema_nodes.first
          )
        end

        def register_source(node, source)
          @node_sources[node] = source
          node.children.each { |child| register_source(child, source) }
        end

        def inherit_source(new_node, old_node)
          @node_sources[new_node] = @node_sources[old_node]
          new_node
        end

        def location_of(node)
          source = @node_sources[node]
          if node.line && node.col
            "#{source}:#{node.line}:#{node.col}"
          else
            source.to_s
          end
        end

        def kind_name(node)
          node.class.name.split("::").last.gsub(/([a-z])([A-Z])/, '\1 \2').downcase
        end

        def named_type_name(type)
          type.respond_to?(:name) ? type.name : type
        end

        def type_string(type)
          case type
          when Nodes::NonNullType
            "#{type_string(type.of_type)}!"
          when Nodes::ListType
            "[#{type_string(type.of_type)}]"
          when Nodes::TypeName
            type.name
          else
            type.to_s
          end
        end

        def default_string(value)
          value.is_a?(Nodes::AbstractNode) ? value.to_query_string : value.inspect
        end
      end

      private_constant :Merger
    end
  end
end
