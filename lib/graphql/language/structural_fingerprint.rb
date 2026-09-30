# frozen_string_literal: true

require "digest/sha2"
require "base64"
require "set"

module GraphQL
  module Language
    # Builds a structural fingerprint for an executable GraphQL document.
    #
    # Unlike a raw hash of the query string, this identifies queries that are
    # structurally equivalent under a small, conservative set of rules:
    #
    # - Variable definitions and every corresponding {Nodes::VariableIdentifier}
    #   are consistently renamed to canonical names ($v0, $v1, ...).
    # - Fragment definitions and every corresponding {Nodes::FragmentSpread}
    #   are consistently renamed to canonical names (F0, F1, ...).
    # - Arguments on fields and directives, and the fields of input object
    #   literals, are sorted by name (their textual order is insignificant).
    # - Whitespace and other incidental formatting differences are ignored,
    #   since the fingerprint is derived from a reprinted canonical document.
    #
    # The following are deliberately preserved, so that changing them always
    # produces a different fingerprint:
    #
    # - Response aliases ({Nodes::Field#alias}).
    # - The order of selected fields.
    # - The order of elements in list literals.
    # - The order of directives (repeated or otherwise), which may be meaningful.
    # - Literal values, including custom scalar literals. Values are compared
    #   syntactically only; custom scalar coercion is never invoked, so `1` and
    #   `1.0` (or `"2024-01-01"` and a date scalar) are never guessed equal.
    # - Operation names, operation types, type conditions, variable types and
    #   default values.
    #
    # The input AST is never modified; all normalization happens on freshly
    # built nodes.
    #
    # @example
    #   doc_a = GraphQL.parse("query($x: ID!){ node(id: $x) { name } }")
    #   doc_b = GraphQL.parse("query($y: ID!){ node(id: $y){ name } }")
    #   GraphQL::Language::StructuralFingerprint.generate(doc_a) ==
    #     GraphQL::Language::StructuralFingerprint.generate(doc_b) # => true
    module StructuralFingerprint
      VERSION_PREFIX = "structural/v1"

      # The normalized document and the fingerprint derived from it.
      class Result
        # @return [String] Opaque, stable structural fingerprint
        attr_reader :fingerprint
        # @return [Nodes::Document] A freshly built, normalized document
        attr_reader :document

        def initialize(fingerprint:, document:)
          @fingerprint = fingerprint
          @document = document
        end

        # @return [String] The canonical query string backing the fingerprint
        def normalized_query_string
          @normalized_query_string ||= @document.to_query_string
        end
      end

      # @param document_or_string [Nodes::Document, String] A parsed executable
      #   document, or a query string to parse first
      # @return [String] A stable, opaque fingerprint for the document's structure
      def self.generate(document_or_string)
        analyze(document_or_string).fingerprint
      end

      # @param document_or_string [Nodes::Document, String] A parsed executable
      #   document, or a query string to parse first
      # @return [Nodes::Document] A normalized copy; the input document is left untouched
      def self.normalize(document_or_string)
        analyze(document_or_string).document
      end

      # @param document_or_string [Nodes::Document, String] A parsed executable
      #   document, or a query string to parse first
      # @return [Result]
      def self.analyze(document_or_string)
        document =
          case document_or_string
          when Nodes::Document
            document_or_string
          when String
            GraphQL.parse(document_or_string)
          else
            raise ArgumentError,
              "expected a GraphQL::Language::Nodes::Document or String, got #{document_or_string.class}"
          end
        Normalizer.new(document).result
      end

      # @api private
      class Normalizer
        def initialize(document)
          @document = document
        end

        def result
          normalized = normalize_document
          canonical_string = normalized.to_query_string
          digest = Digest::SHA256.digest(canonical_string)
          fingerprint = "#{VERSION_PREFIX}/#{Base64.urlsafe_encode64(digest)}"
          Result.new(fingerprint: fingerprint, document: normalized)
        end

        private

        # Normalize a document into independent, self-contained units: one per
        # operation (including the transitive closure of fragments it reaches),
        # followed by any fragment definitions reached by no operation.
        def normalize_document
          fragments = @document.definitions.grep(Nodes::FragmentDefinition)
          fragments_by_name = {}
          fragments.each { |frag| fragments_by_name[frag.name] = frag }

          operations = @document.definitions.grep(Nodes::OperationDefinition)

          units = operations.map { |op| build_unit(op, fragments_by_name) }

          reached = Set.new
          operations.each do |op|
            fragment_bfs(op.selections, fragments_by_name).each do |frag|
              reached << frag.name
            end
          end

          # Preserve document order for fragments that no operation references.
          fragments.each do |frag|
            next if reached.include?(frag.name)
            units << build_unit(frag, fragments_by_name)
          end

          Nodes::Document.new(definitions: units.flatten)
        end

        # @return [Array<Nodes::AbstractNode>] the normalized root plus its fragments
        def build_unit(root, fragments_by_name)
          @variable_map = {}
          @variable_count = 0

          reached_fragments = fragment_bfs(root.selections, fragments_by_name)
          @fragment_map = {}
          reached_fragments.each_with_index do |frag, idx|
            @fragment_map[frag.name] = "F#{idx}"
          end

          bind_variables!(root, reached_fragments)

          normalized_root = transform_definition(root)
          normalized_fragments = reached_fragments.map { |frag| transform_definition(frag) }
          [normalized_root, *normalized_fragments]
        end

        # Breadth-first collection of fragments spread from `selections`,
        # following transitive spreads. Order is deterministic.
        def fragment_bfs(selections, fragments_by_name)
          ordered = []
          seen = Set.new
          queue = selections.dup
          until queue.empty?
            node = queue.shift
            case node
            when Nodes::FragmentSpread
              next if seen.include?(node.name)
              frag = fragments_by_name[node.name]
              next if frag.nil?
              seen << node.name
              ordered << frag
              queue.concat(frag.selections)
            when Nodes::Field
              queue.concat(node.selections)
            when Nodes::InlineFragment
              queue.concat(node.selections)
            end
          end
          ordered
        end

        # Assign canonical variable names. Declared operation variables are
        # bound first (in declaration order); any remaining identifiers are
        # bound in body traversal order.
        def bind_variables!(root, reached_fragments)
          if root.is_a?(Nodes::OperationDefinition)
            root.variables.each do |var_def|
              bind_variable!(var_def.name)
            end
          end

          bodies = [root, *reached_fragments]
          bodies.each do |body|
            scan_variables(body.selections) do |identifier|
              bind_variable!(identifier.name)
            end
          end
        end

        def bind_variable!(name)
          return if @variable_map.key?(name)
          @variable_map[name] = "v#{@variable_count}"
          @variable_count += 1
        end

        def scan_variables(selections, &block)
          selections.each do |selection|
            case selection
            when Nodes::Field
              selection.arguments.each { |arg| scan_value_variables(arg.value, &block) }
              selection.directives.each { |directive| scan_directive_variables(directive, &block) }
              scan_variables(selection.selections, &block)
            when Nodes::InlineFragment
              selection.directives.each { |directive| scan_directive_variables(directive, &block) }
              scan_variables(selection.selections, &block)
            when Nodes::FragmentSpread
              selection.directives.each { |directive| scan_directive_variables(directive, &block) }
            end
          end
        end

        def scan_value_variables(value, &block)
          case value
          when Nodes::VariableIdentifier
            yield value
          when Nodes::InputObject
            value.arguments.each { |arg| scan_value_variables(arg.value, &block) }
          when Array
            value.each { |item| scan_value_variables(item, &block) }
          end
        end

        def scan_directive_variables(directive, &block)
          directive.arguments.each { |arg| scan_value_variables(arg.value, &block) }
        end

        def transform_definition(node)
          case node
          when Nodes::OperationDefinition
            Nodes::OperationDefinition.new(
              operation_type: node.operation_type,
              name: node.name,
              variables: node.variables.map { |var_def| transform_variable_definition(var_def) },
              directives: node.directives.map { |directive| transform_directive(directive) },
              selections: node.selections.map { |selection| transform_selection(selection) },
            )
          when Nodes::FragmentDefinition
            Nodes::FragmentDefinition.new(
              name: @fragment_map.fetch(node.name, node.name),
              type: transform_type(node.type),
              directives: node.directives.map { |directive| transform_directive(directive) },
              selections: node.selections.map { |selection| transform_selection(selection) },
            )
          end
        end

        def transform_variable_definition(node)
          Nodes::VariableDefinition.new(
            name: @variable_map.fetch(node.name, node.name),
            type: transform_type(node.type),
            default_value: node.default_value.nil? ? nil : transform_value(node.default_value),
            directives: node.directives.map { |directive| transform_directive(directive) },
          )
        end

        def transform_selection(node)
          case node
          when Nodes::Field
            Nodes::Field.new(
              name: node.name,
              field_alias: node.alias,
              arguments: normalize_arguments(node.arguments),
              directives: node.directives.map { |directive| transform_directive(directive) },
              selections: node.selections.map { |selection| transform_selection(selection) },
            )
          when Nodes::FragmentSpread
            Nodes::FragmentSpread.new(
              name: @fragment_map.fetch(node.name, node.name),
              directives: node.directives.map { |directive| transform_directive(directive) },
            )
          when Nodes::InlineFragment
            Nodes::InlineFragment.new(
              type: node.type.nil? ? nil : transform_type(node.type),
              directives: node.directives.map { |directive| transform_directive(directive) },
              selections: node.selections.map { |selection| transform_selection(selection) },
            )
          end
        end

        def transform_directive(node)
          Nodes::Directive.new(
            name: node.name,
            arguments: normalize_arguments(node.arguments),
          )
        end

        # Argument order (and input object field order) is insignificant, so
        # sort by name. Directive order itself is left untouched by callers.
        def normalize_arguments(arguments)
          arguments
            .map { |arg| Nodes::Argument.new(name: arg.name, value: transform_value(arg.value)) }
            .sort_by(&:name)
        end

        def transform_value(value)
          case value
          when Nodes::VariableIdentifier
            Nodes::VariableIdentifier.new(name: @variable_map.fetch(value.name, value.name))
          when Nodes::InputObject
            Nodes::InputObject.new(arguments: normalize_arguments(value.arguments))
          when Nodes::Enum
            Nodes::Enum.new(name: value.name)
          when Nodes::NullValue
            Nodes::NullValue.new(name: value.name)
          when Array
            value.map { |item| transform_value(item) }
          when String, Integer, Float, Symbol, true, false, nil
            value
          else
            value
          end
        end

        def transform_type(node)
          case node
          when Nodes::TypeName
            Nodes::TypeName.new(name: node.name)
          when Nodes::ListType
            Nodes::ListType.new(of_type: transform_type(node.of_type))
          when Nodes::NonNullType
            Nodes::NonNullType.new(of_type: transform_type(node.of_type))
          else
            node
          end
        end
      end
    end
  end
end
