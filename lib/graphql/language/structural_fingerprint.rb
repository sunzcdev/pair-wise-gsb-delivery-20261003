# frozen_string_literal: true

require "digest/sha2"
require "base64"

module GraphQL
  module Language
    # A conservative, syntax-only equivalence fingerprint for a GraphQL document.
    #
    # Unlike {GraphQL::Query::Fingerprint}, which hashes the raw query string,
    # this fingerprint ignores a small, well-defined set of meaningless
    # differences:
    #
    # - Variable names are renamed consistently, based on their declaration
    #   order on the operation (the canonical document uses `$v0`, `$v1`, ...).
    # - Fragment definition names are renamed consistently, based on their
    #   definition order (`f0`, `f1`, ...), including the spreads that refer
    #   to them.
    # - Arguments on fields and directives are sorted by argument name.
    # - Input object fields are recursively sorted by field name. This also
    #   applies to input objects nested in variable default values.
    #
    # Everything that may be semantically meaningful is kept in order:
    #
    # - Response aliases and the order of selection sets.
    # - List (array literal) order, including lists nested in input objects.
    # - Directive order.
    # - Variable definition order (it determines the canonical rename).
    # - Top-level definition order.
    #
    # Custom scalar literals are compared by their printed syntax only; no
    # scalar coercion is performed, so literals such as `"2020-01-01"` and
    # `"2020-01-01T00:00:00Z"` are never guessed equal.
    #
    # The input AST is never modified; {#canonical_document} is a freshly
    # built document.
    #
    # @example
    #   result_a = GraphQL::Language::StructuralFingerprint.generate(query_string_a)
    #   result_b = GraphQL::Language::StructuralFingerprint.generate(query_string_b)
    #   result_a.fingerprint == result_b.fingerprint
    module StructuralFingerprint
      VERSION = "gqlstruct1"

      # A canonicalized document and the fingerprint derived from it.
      class Result
        # @return [String] Stable, opaque fingerprint of the canonical document
        attr_reader :fingerprint

        # @return [GraphQL::Language::Nodes::Document] A new AST
        attr_reader :canonical_document

        def initialize(fingerprint:, canonical_document:)
          @fingerprint = fingerprint
          @canonical_document = canonical_document
        end

        # @return [String] The printed canonical document
        def canonical_query_string
          @canonical_query_string ||= @canonical_document.to_query_string
        end
      end

      class << self
        # Build a structural fingerprint for a query string or parsed document.
        #
        # @param query [String, GraphQL::Language::Nodes::Document]
        # @return [StructuralFingerprint::Result]
        def generate(query)
          document =
            case query
            when Nodes::Document
              query
            when String
              GraphQL.parse(query)
            else
              raise ArgumentError, "expected a String or GraphQL::Language::Nodes::Document, got #{query.class}"
            end

          normalized = Normalizer.new(document).normalize
          canonical_query_string = normalized.to_query_string
          digest = Digest::SHA256.digest(canonical_query_string)
          fingerprint = "#{VERSION}/#{Base64.urlsafe_encode64(digest)}"
          Result.new(
            fingerprint: fingerprint,
            canonical_document: normalized,
          )
        end
      end

      # Builds a new, normalized AST without touching the input document.
      # @api private
      class Normalizer
        EMPTY_RENAME = {}.freeze

        def initialize(document)
          @document = document
        end

        def normalize
          fragment_rename = build_fragment_rename
          variable_renames = build_variable_renames

          new_definitions = @document.definitions.map do |definition|
            case definition
            when Nodes::OperationDefinition
              rewrite_operation(definition, fragment_rename, variable_renames[definition])
            when Nodes::FragmentDefinition
              rewrite_fragment_definition(definition, fragment_rename, variable_renames[definition])
            else
              rewrite_node(definition, EMPTY_RENAME)
            end
          end
          Nodes::Document.new(definitions: new_definitions)
        end

        private

        # Fragment names are renamed in definition order.
        def build_fragment_rename
          fragment_rename = {}
          @document.definitions.each do |definition|
            if definition.is_a?(Nodes::FragmentDefinition)
              fragment_rename[definition.name] = "f#{fragment_rename.size}"
            end
          end
          fragment_rename
        end

        # Variables are renamed in declaration order, per operation. When
        # several operations use one fragment, the fragment's variables are
        # only renamed when every such operation assigns the same canonical
        # name; otherwise the fragment keeps its original variable names.
        def build_variable_renames
          operations = @document.definitions.grep(Nodes::OperationDefinition)
          fragments_by_name = {}
          @document.definitions.each do |definition|
            fragments_by_name[definition.name] = definition if definition.is_a?(Nodes::FragmentDefinition)
          end

          operation_renames = {}
          operations.each do |operation|
            rename = {}
            operation.variables.each_with_index do |variable, idx|
              rename[variable.name] = "v#{idx}"
            end
            operation_renames[operation] = rename
          end

          fragment_candidates = {}
          operations.each do |operation|
            used_fragment_names(operation.selections, fragments_by_name).each do |fragment_name|
              (fragment_candidates[fragment_name] ||= []) << operation_renames[operation]
            end
          end

          variable_renames = {}
          fragment_candidates.each do |fragment_name, renames|
            variable_renames[fragments_by_name[fragment_name]] = merge_agreed_renames(renames)
          end
          fragments_by_name.each_value do |fragment|
            variable_renames[fragment] ||= EMPTY_RENAME
          end

          operation_renames.merge(variable_renames)
        end

        # @return [Hash] canonical rename when every scope agrees, otherwise an empty map
        def merge_agreed_renames(renames)
          agreed = {}
          renames.each do |rename|
            rename.each do |original_name, canonical_name|
              if (existing = agreed[original_name]) && existing != canonical_name
                return EMPTY_RENAME
              end
              agreed[original_name] = canonical_name
            end
          end
          agreed
        end

        # Fragment names transitively used by a selection set.
        def used_fragment_names(selections, fragments_by_name, seen = {})
          selections.each do |selection|
            case selection
            when Nodes::FragmentSpread
              next if seen.key?(selection.name)
              seen[selection.name] = true
              referenced_fragment = fragments_by_name[selection.name]
              if referenced_fragment
                used_fragment_names(referenced_fragment.selections, fragments_by_name, seen)
              end
            when Nodes::Field
              used_fragment_names(selection.selections, fragments_by_name, seen)
            when Nodes::InlineFragment
              used_fragment_names(selection.selections, fragments_by_name, seen)
            end
          end
          seen.keys
        end

        def rewrite_operation(operation, fragment_rename, variable_rename)
          Nodes::OperationDefinition.new(
            operation_type: operation.operation_type,
            name: operation.name,
            variables: operation.variables.map { |variable| rewrite_variable_definition(variable, variable_rename) },
            directives: rewrite_directives(operation.directives, variable_rename),
            selections: rewrite_selections(operation.selections, variable_rename, fragment_rename),
          )
        end

        def rewrite_fragment_definition(fragment, fragment_rename, variable_rename)
          Nodes::FragmentDefinition.new(
            name: fragment_rename.fetch(fragment.name, fragment.name),
            type: fragment.type,
            directives: rewrite_directives(fragment.directives, variable_rename),
            selections: rewrite_selections(fragment.selections, variable_rename, fragment_rename),
          )
        end

        def rewrite_variable_definition(variable, variable_rename)
          Nodes::VariableDefinition.new(
            name: variable_rename.fetch(variable.name, variable.name),
            type: variable.type,
            default_value: rewrite_value(variable.default_value, variable_rename),
            directives: rewrite_directives(variable.directives, variable_rename),
          )
        end

        def rewrite_selections(selections, variable_rename, fragment_rename)
          selections.map do |selection|
            case selection
            when Nodes::Field
              rewrite_field(selection, variable_rename, fragment_rename)
            when Nodes::FragmentSpread
              rewrite_fragment_spread(selection, variable_rename, fragment_rename)
            when Nodes::InlineFragment
              rewrite_inline_fragment(selection, variable_rename, fragment_rename)
            else
              rewrite_node(selection, EMPTY_RENAME)
            end
          end
        end

        def rewrite_field(field, variable_rename, fragment_rename)
          Nodes::Field.new(
            name: field.name,
            field_alias: field.alias,
            arguments: rewrite_arguments(field.arguments, variable_rename),
            directives: rewrite_directives(field.directives, variable_rename),
            selections: rewrite_selections(field.selections, variable_rename, fragment_rename),
          )
        end

        def rewrite_fragment_spread(spread, variable_rename, fragment_rename)
          Nodes::FragmentSpread.new(
            name: fragment_rename.fetch(spread.name, spread.name),
            directives: rewrite_directives(spread.directives, variable_rename),
          )
        end

        def rewrite_inline_fragment(inline_fragment, variable_rename, fragment_rename)
          Nodes::InlineFragment.new(
            type: inline_fragment.type,
            directives: rewrite_directives(inline_fragment.directives, variable_rename),
            selections: rewrite_selections(inline_fragment.selections, variable_rename, fragment_rename),
          )
        end

        # Directive order is significant; only the directive's arguments are
        # sorted.
        def rewrite_directives(directives, variable_rename)
          directives.map do |directive|
            Nodes::Directive.new(
              name: directive.name,
              arguments: rewrite_arguments(directive.arguments, variable_rename),
            )
          end
        end

        # Argument order is meaningless, so sort by name. Duplicate argument
        # names (invalid GraphQL) keep their relative order.
        def rewrite_arguments(arguments, variable_rename)
          arguments
            .each_with_index
            .sort_by { |argument, idx| [argument.name, idx] }
            .map { |argument, _idx| rewrite_argument(argument, variable_rename) }
        end

        def rewrite_argument(argument, variable_rename)
          Nodes::Argument.new(
            name: argument.name,
            value: rewrite_value(argument.value, variable_rename),
          )
        end

        # List order is preserved; only input objects nested inside the list
        # get their fields sorted.
        def rewrite_value(value, variable_rename)
          case value
          when Nodes::InputObject
            rewrite_input_object(value, variable_rename)
          when Nodes::VariableIdentifier
            Nodes::VariableIdentifier.new(name: variable_rename.fetch(value.name, value.name))
          when ::Array
            value.map { |item| rewrite_value(item, variable_rename) }
          else
            value
          end
        end

        def rewrite_input_object(input_object, variable_rename)
          Nodes::InputObject.new(
            arguments: rewrite_arguments(input_object.arguments, variable_rename),
          )
        end

        # Fallback for definition kinds outside executable documents (for
        # example SDL): keep structure intact while still sorting arguments.
        def rewrite_node(node, variable_rename)
          case node
          when Nodes::Directive
            Nodes::Directive.new(
              name: node.name,
              arguments: rewrite_arguments(node.arguments, variable_rename),
            )
          when Nodes::Argument
            rewrite_argument(node, variable_rename)
          when Nodes::VariableIdentifier
            Nodes::VariableIdentifier.new(name: variable_rename.fetch(node.name, node.name))
          when Nodes::InputObject
            rewrite_input_object(node, variable_rename)
          when ::Array
            node.map { |item| rewrite_node(item, variable_rename) }
          else
            node
          end
        end
      end
    end
  end
end
