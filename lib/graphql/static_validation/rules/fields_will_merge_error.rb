# frozen_string_literal: true
module GraphQL
  module StaticValidation
    class FieldsWillMergeError < StaticValidation::Error
      attr_reader :field_name
      attr_reader :kind
      # Structured diagnostics explaining how the conflicting fields met at
      # this response position. Each entry is a Hash with:
      #
      # - `"responsePath"`: response keys (aliases included) from the
      #   operation root to the conflicting field
      # - `"reachableOn"`: names of the concrete object types on which both
      #   sides of the conflict can be selected at the same time (empty when
      #   the type conditions are mutually exclusive)
      # - `"sides"`: one entry per conflicting field, with its field name,
      #   alias (if any), location, and the `"via"` chain of fragments and
      #   inline fragments it was reached through
      # - `"mutuallyExclusive"` (optional): `true` when the two sides can
      #   never co-occur on the same object (only possible for `return_type`
      #   conflicts, which are reported regardless of overlap)
      attr_reader :conflict_evidence

      def initialize(kind:, field_name:)
        super(nil)

        @field_name = field_name
        @kind = kind
        @conflicts = []
        @conflict_evidence = []
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

      def add_conflict(node, conflict_str)
        # Check if we already have an error for this exact node.
        # Use object identity first (fast path), then fall back to
        # value + location comparison for duplicate AST nodes.
        if nodes.any? { |n| n.equal?(node) || (n.line == node.line && n.col == node.col && n == node) }
          # already have an error for this node
          return
        end

        @nodes << node
        @conflicts << conflict_str
      end

      # Attach structured diagnostics for one pair of conflicting fields.
      # @param evidence [Hash] see {#conflict_evidence} for the shape
      def add_evidence(evidence)
        @conflict_evidence << evidence
      end

      # A hash representation of this Message
      def to_h
        extensions = {
          "code" => code,
          "fieldName" => field_name,
          "conflicts" => conflicts
        }

        if !@conflict_evidence.empty?
          extensions["conflictEvidence"] = @conflict_evidence
        end

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
