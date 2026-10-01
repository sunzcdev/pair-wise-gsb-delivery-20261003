# frozen_string_literal: true
module GraphQL
  module StaticValidation
    class FieldsWillMergeError < StaticValidation::Error
      attr_reader :field_name
      attr_reader :kind

      def initialize(kind:, field_name:)
        super(nil)

        @field_name = field_name
        @kind = kind
        @conflicts = []
        @sides = []
        @response_path = nil
      end

      def message
        @message || "Field '#{field_name}' has #{kind == :argument ? 'an' : 'a'} #{kind} conflict: #{conflicts}?"
      end

      attr_writer :message

      def path
        []
      end

      def conflicts
        @conflicts.join(' or ')
      end

      def add_conflict(node, conflict_str, path: nil, via: nil, possible_types: nil)
        # Check if we already have an error for this exact node.
        # Use object identity first (fast path), then fall back to
        # value + location comparison for duplicate AST nodes.
        if nodes.any? { |n| n.equal?(node) || (n.line == node.line && n.col == node.col && n == node) }
          # already have an error for this node
          return
        end

        @nodes << node
        @conflicts << conflict_str
        @response_path ||= path

        side = { "field" => node.name }
        if (node_alias = node.alias)
          side["alias"] = node_alias
        end
        side["via"] = via || []
        side["possibleTypes"] = possible_types || []
        @sides << side
      end

      # Structured evidence explaining why the conflicting fields meet at the
      # same response position:
      #
      # - `"responsePath"`: the response-key path (using aliases, if any) from
      #   the operation root to the conflicting response name. When the
      #   conflict was detected inside a fragment definition, the path is
      #   relative to that fragment, since the fragment may be spread at
      #   several response positions.
      # - `"sharedPossibleTypes"`: the concrete object types on which _all_
      #   conflicting sides may be selected at once. When the sides' type
      #   conditions are mutually exclusive, this is empty -- no common
      #   object type exists, so no shared-type evidence is invented.
      # - `"sides"`: one entry per conflicting field occurrence, with the
      #   fragment chain (`"via"`) it was reached through and the object
      #   types (`"possibleTypes"`) it applies to.
      def diagnostics
        shared_possible_types = nil
        @sides.each do |side|
          side_types = side["possibleTypes"]
          shared_possible_types = shared_possible_types.nil? ? side_types : (shared_possible_types & side_types)
        end

        {
          "responsePath" => @response_path || [],
          "sharedPossibleTypes" => shared_possible_types || [],
          "sides" => @sides,
        }
      end

      # A hash representation of this Message
      def to_h
        extensions = {
          "code" => code,
          "fieldName" => field_name,
          "conflicts" => conflicts,
          "diagnostics" => diagnostics,
        }

        super.merge({
          "extensions" => extensions
        })
      end

      def code
        "fieldConflict"
      end
    end
  end
end
