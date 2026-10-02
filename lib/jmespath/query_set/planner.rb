# frozen_string_literal: true

module JMESPath
  class QuerySet
    # @api private
    #
    # Compiles a collection of named expressions into a fused plan.
    #
    # The planner is purely structural. It groups plan entries whose *first
    # step* is semantically identical (same provenance marker and same
    # structural fingerprint), evaluates that shared head exactly once, and
    # then recursively fuses the remaining tails. Projection targets and
    # per-element branches, filter tests, operands and function arguments
    # are fused through the same mechanism.
    class Planner
      # A single entry flowing through a {LevelPlan}.
      #   id    - routing key (a query id or an internal slot id)
      #   owners- query ids that depend on this entry (error attribution)
      #   steps - remaining linearized {Step}s
      Entry = Struct.new(:id, :owners, :steps)

      # A fused level: every entry here is evaluated against the SAME input.
      LevelPlan = Struct.new(:groups, :owners) do
        def each_entry_id
          groups.each { |g| g.entry_ids.each { |id| yield id } }
        end
      end

      # One group of entries with an identical head.
      Group = Struct.new(
        :type, :node, :entry_ids, :owners,
        :internal_level, :slot_ids,          # shared internal children
        :next_level,                         # passthrough groups
        :branch_level, :member_tails,        # projections
        :child_level,                        # conditions
        :arm_level,                          # and/or right arm
        :pair_keys                           # multi-select hash keys
      )

      IDENTITY = Object.new.freeze

      def initialize
        @slot_seq = 0
      end

      # @param entries [Array<Entry>]
      # @return [LevelPlan]
      def compile(entries)
        grouped = []
        entries.each do |entry|
          key = entry.steps.empty? ? IDENTITY : group_key(entry.steps.first.node, entry.steps.first.marker)
          bucket = grouped.find { |b| b[:key] == key }
          unless bucket
            bucket = { key: key, node: entry.steps.first&.node, members: [] }
            grouped << bucket
          end
          bucket[:members] << entry
        end

        groups = grouped.map { |bucket| build_group(bucket) }
        LevelPlan.new(groups, owners_map(groups))
      end

      # Static map of entry_id => owning query ids for a level's groups.
      def owners_map(groups)
        groups.each_with_object({}) { |g, h| g.entry_ids.each { |id| h[id] = g.owners } }
      end

      private

      # Semantic head key. Provenance marker is always included so a dotted
      # prefix never merges across a pipe boundary.
      def group_key(node, marker)
        k = AST.kind(node)
        case k
        when :array_projection, :object_projection
          [k, marker, AST.fingerprint(node.target)]
        when :condition
          [:condition, marker, AST.fingerprint(node.test)]
        when :and, :or
          [k, marker, AST.fingerprint(node.left)]
        when :flatten
          [:flatten, marker, AST.fingerprint(node.child)]
        when :not
          [:not, marker, AST.fingerprint(node.expression)]
        else
          # function / comparator / multiselect / expression / leaf:
          # the whole node (operands included) defines the head.
          [:full, k, marker, AST.fingerprint(node)]
        end
      end

      def build_group(bucket)
        node = bucket[:node]
        members = bucket[:members]
        entry_ids = members.map(&:id)
        owners = members.flat_map(&:owners).uniq
        type = node.nil? ? :identity : AST.kind(node)

        group = Group.new
        group.type = type
        group.node = node
        group.entry_ids = entry_ids
        group.owners = owners

        case type
        when :identity
          # no steps left: value passes through unchanged
          group
        when :array_projection, :object_projection
          build_projection(group, members)
        when :condition
          build_condition(group, members)
        when :and, :or
          build_gate(group, members)
        when :multi_select_hash
          group.pair_keys = node.kv_pairs.map(&:key)
          build_passthrough(group, members)
        when :multi_select_list, :function, :comparator, :flatten, :not, :leaf, :expression
          build_passthrough(group, members)
        when :subexpression
          raise 'subexpression nodes must have been linearized'
        else
          build_passthrough(group, members)
        end
        group
      end

      # Shared internal children, evaluated against the level input.
      def build_internals(group, child_asts, members)
        return nil, [] if child_asts.empty?

        slot_ids = child_asts.map.with_index { |_, pos| new_slot_id(group, pos) }
        entries = child_asts.each_with_index.map do |ast, pos|
          Entry.new(slot_ids[pos], members.flat_map(&:owners).uniq, Normalizer.to_steps(ast))
        end
        [compile(entries), slot_ids]
      end

      def build_passthrough(group, members)
        child_asts = AST.internals(group.node)
        group.internal_level, group.slot_ids = build_internals(group, child_asts, members)
        # members share the single head output, so their tails stay fused
        group.next_level = compile(members.map { |m| Entry.new(m.id, m.owners, m.steps.drop(1)) })
      end

      def build_projection(group, members)
        target = group.node.target
        group.internal_level, group.slot_ids = build_internals(group, [target], members)

        # branches are evaluated per projected element, all sharing the
        # element, so they are fused together
        branch_entries = members.map do |m|
          branch_node = m.steps.first.node.projection
          Entry.new(m.id, m.owners, Normalizer.to_steps(branch_node))
        end
        group.branch_level = compile(branch_entries)

        # after collection each member owns a different list, so tails solo
        group.member_tails = members.each_with_object({}) do |m, tails|
          tails[m.id] = compile([Entry.new(m.id, m.owners, m.steps.drop(1))])
        end
      end

      def build_condition(group, members)
        group.internal_level, group.slot_ids = build_internals(group, [group.node.test], members)

        child_entries = members.map do |m|
          child_node = m.steps.first.node.child
          Entry.new(m.id, m.owners, Normalizer.to_steps(child_node))
        end
        group.child_level = compile(child_entries)

        group.member_tails = members.each_with_object({}) do |m, tails|
          tails[m.id] = compile([Entry.new(m.id, m.owners, m.steps.drop(1))])
        end
      end

      def build_gate(group, members)
        node = group.node
        group.internal_level, group.slot_ids = build_internals(group, [node.left], members)

        arm_entries = members.map { |m| Entry.new(m.id, m.owners, Normalizer.to_steps(node.right)) }
        group.arm_level = compile(arm_entries)

        group.member_tails = members.each_with_object({}) do |m, tails|
          tails[m.id] = compile([Entry.new(m.id, m.owners, m.steps.drop(1))])
        end
      end

      def new_slot_id(group, pos)
        @slot_seq += 1
        [:slot, @slot_seq, pos]
      end
    end
  end
end
