# frozen_string_literal: true

module GraphQL
  module Language
    # Merges several parsed SDL documents into a single {GraphQL::Language::Nodes::Document}.
    #
    # Each document is tagged with a source identifier (a file name, a module name, ...)
    # so that merge errors can point at _both_ conflicting declarations:
    #
    # @example Merging two documents and building a schema from the result
    #   merged = GraphQL::Language::DocumentMerger.merge([
    #     { source: "graphql/users.graphql", document: GraphQL.parse(users_sdl) },
    #     { source: "graphql/billing.graphql", document: GraphQL.parse(billing_sdl) },
    #   ])
    #   schema = GraphQL::Schema.from_definition(merged.to_query_string)
    #
    # The merge is structural, not textual:
    #
    # - Each type may be _defined_ only once across all documents, but it may be
    #   _extended_ by any document. Extensions are folded into their definition,
    #   so cross-document references (a type used in one document but declared in
    #   another) are resolved only after all documents have been read.
    # - Duplicate fields are checked field-by-field: conflicting field types or
    #   conflicting argument default values raise {MergeError}; identical
    #   declarations are merged silently.
    # - Root operation types (`schema { query: ... }`) and `directive` definitions
    #   have their own compatibility rules -- nothing is silently overwritten.
    #
    # The input ASTs are never modified; the returned document is built from copies.
    class DocumentMerger
      # Raised when the given documents can't be merged into one valid document.
      # The message names both sources and the position of each conflicting declaration.
      class MergeError < GraphQL::Error; end

      TYPE_DEFINITION_CLASSES = [
        Nodes::ScalarTypeDefinition,
        Nodes::ObjectTypeDefinition,
        Nodes::InterfaceTypeDefinition,
        Nodes::UnionTypeDefinition,
        Nodes::EnumTypeDefinition,
        Nodes::InputObjectTypeDefinition,
      ].freeze

      EXTENSION_CLASS_FOR = {
        Nodes::ScalarTypeDefinition => Nodes::ScalarTypeExtension,
        Nodes::ObjectTypeDefinition => Nodes::ObjectTypeExtension,
        Nodes::InterfaceTypeDefinition => Nodes::InterfaceTypeExtension,
        Nodes::UnionTypeDefinition => Nodes::UnionTypeExtension,
        Nodes::EnumTypeDefinition => Nodes::EnumTypeExtension,
        Nodes::InputObjectTypeDefinition => Nodes::InputObjectTypeExtension,
      }.freeze

      TYPE_EXTENSION_CLASSES = EXTENSION_CLASS_FOR.values.freeze

      KIND_WORDS = {
        Nodes::ScalarTypeDefinition => "scalar",
        Nodes::ObjectTypeDefinition => "object type",
        Nodes::InterfaceTypeDefinition => "interface",
        Nodes::UnionTypeDefinition => "union",
        Nodes::EnumTypeDefinition => "enum",
        Nodes::InputObjectTypeDefinition => "input object",
        Nodes::ScalarTypeExtension => "scalar extension",
        Nodes::ObjectTypeExtension => "object type extension",
        Nodes::InterfaceTypeExtension => "interface extension",
        Nodes::UnionTypeExtension => "union extension",
        Nodes::EnumTypeExtension => "enum extension",
        Nodes::InputObjectTypeExtension => "input object extension",
      }.freeze

      ROOT_OPERATION_TYPES = ["query", "mutation", "subscription"].freeze

      # Merge the given documents into a single {GraphQL::Language::Nodes::Document}.
      #
      # @param documents [Hash, Array] parsed documents tagged with source identifiers.
      #   Accepts a `{ source => document }` Hash, an Array of `[source, document]` pairs,
      #   or an Array of `{ source: ..., document: ... }` Hashes.
      # @return [GraphQL::Language::Nodes::Document] a new document; the inputs are unchanged.
      # @raise [MergeError] when the documents declare incompatible definitions
      def self.merge(documents)
        new(documents).merge
      end

      def initialize(documents)
        @entries = normalize_entries(documents)
      end

      # @return [GraphQL::Language::Nodes::Document]
      def merge
        collect_definitions
        merged_schema = merge_schema_definitions
        merged_types = merge_type_definitions
        merged_directives = merge_directive_definitions

        definitions = []
        @definition_order.each do |kind, key|
          case kind
          when :schema
            definitions << merged_schema if merged_schema
          when :type
            definitions << merged_types[key]
          when :directive
            definitions << merged_directives[key]
          end
        end

        Nodes::Document.new(definitions: definitions)
      end

      private

      def normalize_entries(documents)
        pairs = case documents
        when Hash
          documents.map { |source, document| [source, document] }
        when Array
          documents.map do |entry|
            case entry
            when Hash
              [entry.fetch(:source), entry.fetch(:document)]
            when Array
              source, document = entry
              [source, document]
            else
              raise ArgumentError, "Expected { source: ..., document: ... } or [source, document], got: #{entry.inspect}"
            end
          end
        else
          raise ArgumentError, "Expected a Hash or Array of documents, got: #{documents.class}"
        end

        if pairs.empty?
          raise ArgumentError, "Must provide at least one document to merge"
        end

        pairs.each do |source, document|
          if !document.is_a?(Nodes::Document)
            raise ArgumentError, "Expected a GraphQL::Language::Nodes::Document for #{source.inspect}, got: #{document.class}"
          end
        end

        pairs
      end

      def collect_definitions
        @schema_definitions = []
        @schema_extensions = []
        @type_definitions = Hash.new { |h, k| h[k] = [] }
        @type_extensions = Hash.new { |h, k| h[k] = [] }
        @directive_definitions = Hash.new { |h, k| h[k] = [] }
        @definition_order = []

        @entries.each do |source, document|
          document.definitions.each do |definition|
            case definition
            when Nodes::SchemaDefinition
              @definition_order << [:schema] if @schema_definitions.empty? && @schema_extensions.empty?
              @schema_definitions << [source, definition]
            when Nodes::SchemaExtension
              @definition_order << [:schema] if @schema_definitions.empty? && @schema_extensions.empty?
              @schema_extensions << [source, definition]
            when Nodes::DirectiveDefinition
              @definition_order << [:directive, definition.name] if !@directive_definitions.key?(definition.name)
              @directive_definitions[definition.name] << [source, definition]
            when *TYPE_DEFINITION_CLASSES
              @definition_order << [:type, definition.name] if !@type_definitions.key?(definition.name)
              @type_definitions[definition.name] << [source, definition]
            when *TYPE_EXTENSION_CLASSES
              @type_extensions[definition.name] << [source, definition]
            else
              raise MergeError, "Cannot merge documents: #{definition.class.name.split("::").last} at #{location_string(source, definition)} is not a type system definition. Only SDL documents can be merged."
            end
          end
        end
      end

      # Root operation types and schema directives may be spread over one
      # `schema` definition and any number of `extend schema` blocks.
      # Each operation type must agree wherever it appears.
      def merge_schema_definitions
        return nil if @schema_definitions.empty? && @schema_extensions.empty?

        if @schema_definitions.size > 1
          (first_source, first_node), (second_source, second_node) = @schema_definitions
          raise MergeError, "Cannot merge documents: schema is defined more than once, at #{location_string(first_source, first_node)} and #{location_string(second_source, second_node)}."
        end

        entries = @schema_definitions + @schema_extensions
        operations = {}
        directives = []

        entries.each do |source, node|
          ROOT_OPERATION_TYPES.each do |operation_type|
            type_name = node.public_send(operation_type)
            next if type_name.nil?

            if (previous = operations[operation_type])
              previous_type, previous_source, previous_node = previous
              if previous_type != type_name
                raise MergeError, "Cannot merge documents: conflicting root operation types for \"#{operation_type}\": \"#{previous_type}\" at #{location_string(previous_source, previous_node)} and \"#{type_name}\" at #{location_string(source, node)}."
              end
            else
              operations[operation_type] = [type_name, source, node]
            end
          end

          node.directives.each do |directive|
            directives << directive if directives.none? { |d| d == directive }
          end
        end

        if @schema_extensions.empty?
          @schema_definitions.first[1]
        elsif (base_entry = @schema_definitions.first)
          base_entry[1].merge(
            query: operations["query"]&.[](0),
            mutation: operations["mutation"]&.[](0),
            subscription: operations["subscription"]&.[](0),
            directives: directives,
          )
        else
          Nodes::SchemaDefinition.new(
            query: operations["query"]&.[](0),
            mutation: operations["mutation"]&.[](0),
            subscription: operations["subscription"]&.[](0),
            directives: directives,
          )
        end
      end

      def merge_type_definitions
        # Extensions may reference types declared in _other_ documents,
        # so check them only now that every definition has been collected.
        @type_extensions.each do |name, extensions|
          definition_entries = @type_definitions[name]
          if definition_entries.nil? || definition_entries.empty?
            source, node = extensions.first
            raise MergeError, "Cannot merge documents: #{kind_word(node)} \"#{name}\" at #{location_string(source, node)} has no matching type definition in any merged document."
          end
        end

        merged = {}
        @type_definitions.each do |name, entries|
          if entries.size > 1
            (first_source, first_node), (second_source, second_node) = entries
            if first_node.class != second_node.class
              raise MergeError, "Cannot merge documents: \"#{name}\" is defined as #{indefinite(kind_word(first_node))} at #{location_string(first_source, first_node)} but as #{indefinite(kind_word(second_node))} at #{location_string(second_source, second_node)}."
            else
              raise MergeError, "Cannot merge documents: #{kind_word(first_node)} \"#{name}\" is defined more than once, at #{location_string(first_source, first_node)} and #{location_string(second_source, second_node)}."
            end
          end

          definition_source, definition = entries.first
          extensions = @type_extensions[name] || []
          expected_extension_class = EXTENSION_CLASS_FOR[definition.class]
          extensions.each do |extension_source, extension|
            if !extension.is_a?(expected_extension_class)
              raise MergeError, "Cannot merge documents: #{kind_word(definition)} \"#{name}\" at #{location_string(definition_source, definition)} can't be extended by #{indefinite(kind_word(extension))} at #{location_string(extension_source, extension)}."
            end
          end

          merged[name] = extensions.empty? ? definition : apply_extensions(name, definition, definition_source, extensions)
        end
        merged
      end

      # Repeated `directive @name ...` definitions are only legal when they
      # are identical: same locations, same `repeatable`, and same arguments
      # (including types and default values).
      def merge_directive_definitions
        merged = {}
        @directive_definitions.each do |name, entries|
          (first_source, first_node) = entries.first
          entries[1..]&.each do |other_source, other_node|
            assert_compatible_directive_definitions(name, first_node, first_source, other_node, other_source)
          end
          merged[name] = first_node
        end
        merged
      end

      def assert_compatible_directive_definitions(name, first_node, first_source, other_node, other_source)
        if first_node.repeatable != other_node.repeatable
          raise MergeError, "Cannot merge documents: directive \"@#{name}\" is #{"not " if first_node.repeatable}repeatable at #{location_string(first_source, first_node)} but #{"not " if other_node.repeatable}repeatable at #{location_string(other_source, other_node)}."
        end

        first_locations = first_node.locations.map(&:name).sort
        other_locations = other_node.locations.map(&:name).sort
        if first_locations != other_locations
          raise MergeError, "Cannot merge documents: directive \"@#{name}\" has locations [#{first_locations.join(", ")}] at #{location_string(first_source, first_node)} but [#{other_locations.join(", ")}] at #{location_string(other_source, other_node)}."
        end

        first_arguments = first_node.arguments.map { |arg| [arg.name, type_string(arg.type), value_string(arg.default_value)] }
        other_arguments = other_node.arguments.map { |arg| [arg.name, type_string(arg.type), value_string(arg.default_value)] }
        if first_arguments != other_arguments
          raise MergeError, "Cannot merge documents: directive \"@#{name}\" has different arguments at #{location_string(first_source, first_node)} and #{location_string(other_source, other_node)}."
        end
      end

      def apply_extensions(name, definition, definition_source, extensions)
        case definition
        when Nodes::ScalarTypeDefinition
          directives = merge_extension_directives(definition, extensions)
          directives.equal?(definition.directives) ? definition : definition.merge(directives: directives)
        when Nodes::ObjectTypeDefinition, Nodes::InterfaceTypeDefinition
          fields = merge_extension_fields(name, definition, definition_source, extensions)
          interfaces = merge_extension_interfaces(definition, extensions)
          directives = merge_extension_directives(definition, extensions)
          definition.merge(fields: fields, interfaces: interfaces, directives: directives)
        when Nodes::UnionTypeDefinition
          types = definition.types.dup
          extensions.each do |_extension_source, extension|
            extension.types.each do |type_name|
              types << type_name if types.none? { |t| t.name == type_name.name }
            end
          end
          directives = merge_extension_directives(definition, extensions)
          definition.merge(types: types, directives: directives)
        when Nodes::EnumTypeDefinition
          values = definition.values.dup
          value_sources = {}
          definition.values.each { |value| value_sources[value.name] = definition_source }
          extensions.each do |extension_source, extension|
            extension.values.each do |value|
              if (previous_source = value_sources[value.name])
                previous_value = values.find { |v| v.name == value.name }
                if previous_value != value
                  raise MergeError, "Cannot merge documents: enum value \"#{name}.#{value.name}\" is defined differently at #{location_string(previous_source, previous_value)} and #{location_string(extension_source, value)}."
                end
              else
                value_sources[value.name] = extension_source
                values << value
              end
            end
          end
          directives = merge_extension_directives(definition, extensions)
          definition.merge(values: values, directives: directives)
        when Nodes::InputObjectTypeDefinition
          fields = merge_extension_input_fields(name, definition, definition_source, extensions)
          directives = merge_extension_directives(definition, extensions)
          definition.merge(fields: fields, directives: directives)
        else
          raise "Invariant: unexpected definition #{definition.class}"
        end
      end

      # Fields added by extensions are checked one by one against the fields
      # that are already present (from the definition or earlier extensions).
      # Identical fields are deduplicated; conflicting ones raise {MergeError}.
      def merge_extension_fields(type_name, definition, definition_source, extensions)
        fields = definition.fields.dup
        field_sources = {}
        fields.each { |field| field_sources[field.name] = definition_source }

        extensions.each do |extension_source, extension|
          extension.fields.each do |field|
            if (previous_source = field_sources[field.name])
              previous_field = fields.find { |f| f.name == field.name }
              assert_compatible_fields(type_name, previous_field, previous_source, field, extension_source)
            else
              field_sources[field.name] = extension_source
              fields << field
            end
          end
        end
        fields
      end

      def assert_compatible_fields(type_name, existing, existing_source, incoming, incoming_source)
        if existing.type != incoming.type
          raise MergeError, "Cannot merge documents: field \"#{type_name}.#{existing.name}\" has conflicting types: \"#{type_string(existing.type)}\" at #{location_string(existing_source, existing)} and \"#{type_string(incoming.type)}\" at #{location_string(incoming_source, incoming)}."
        end

        existing_arguments = existing.arguments.map(&:name)
        incoming_arguments = incoming.arguments.map(&:name)
        if existing_arguments != incoming_arguments
          raise MergeError, "Cannot merge documents: field \"#{type_name}.#{existing.name}\" has arguments (#{existing_arguments.join(", ")}) at #{location_string(existing_source, existing)} but (#{incoming_arguments.join(", ")}) at #{location_string(incoming_source, incoming)}."
        end

        existing.arguments.zip(incoming.arguments).each do |existing_argument, incoming_argument|
          if existing_argument.type != incoming_argument.type
            raise MergeError, "Cannot merge documents: argument \"#{type_name}.#{existing.name}(#{existing_argument.name}:)\" has conflicting types: \"#{type_string(existing_argument.type)}\" at #{location_string(existing_source, existing_argument)} and \"#{type_string(incoming_argument.type)}\" at #{location_string(incoming_source, incoming_argument)}."
          end

          if existing_argument.default_value != incoming_argument.default_value
            raise MergeError, "Cannot merge documents: argument \"#{type_name}.#{existing.name}(#{existing_argument.name}:)\" has conflicting default values: #{value_string(existing_argument.default_value)} at #{location_string(existing_source, existing_argument)} and #{value_string(incoming_argument.default_value)} at #{location_string(incoming_source, incoming_argument)}."
          end
        end
      end

      def merge_extension_input_fields(type_name, definition, definition_source, extensions)
        fields = definition.fields.dup
        field_sources = {}
        fields.each { |field| field_sources[field.name] = definition_source }

        extensions.each do |extension_source, extension|
          extension.fields.each do |field|
            if (previous_source = field_sources[field.name])
              previous_field = fields.find { |f| f.name == field.name }
              if previous_field.type != field.type
                raise MergeError, "Cannot merge documents: input field \"#{type_name}.#{field.name}\" has conflicting types: \"#{type_string(previous_field.type)}\" at #{location_string(previous_source, previous_field)} and \"#{type_string(field.type)}\" at #{location_string(extension_source, field)}."
              end
              if previous_field.default_value != field.default_value
                raise MergeError, "Cannot merge documents: input field \"#{type_name}.#{field.name}\" has conflicting default values: #{value_string(previous_field.default_value)} at #{location_string(previous_source, previous_field)} and #{value_string(field.default_value)} at #{location_string(extension_source, field)}."
              end
            else
              field_sources[field.name] = extension_source
              fields << field
            end
          end
        end
        fields
      end

      def merge_extension_interfaces(definition, extensions)
        interfaces = definition.interfaces.dup
        extensions.each do |_extension_source, extension|
          extension.interfaces.each do |interface|
            interfaces << interface if interfaces.none? { |i| i.name == interface.name }
          end
        end
        interfaces
      end

      def merge_extension_directives(definition, extensions)
        directives = definition.directives.dup
        extensions.each do |_extension_source, extension|
          extension.directives.each do |directive|
            directives << directive if directives.none? { |d| d == directive }
          end
        end
        directives
      end

      def kind_word(node)
        KIND_WORDS.fetch(node.class)
      end

      def indefinite(kind_word)
        "#{kind_word.start_with?("i", "o", "e", "u", "a") ? "an" : "a"} #{kind_word}"
      end

      def location_string(source, node)
        line = node.line
        col = node.col
        if line && col
          "#{source} (line #{line}, column #{col})"
        elsif line
          "#{source} (line #{line})"
        else
          source.to_s
        end
      end

      def type_string(type_node)
        type_node.to_query_string
      end

      def value_string(value)
        case value
        when Nodes::AbstractNode
          value.to_query_string
        when nil
          "null"
        else
          value.inspect
        end
      end
    end
  end
end
