# frozen_string_literal: true

module JMESPath
  class QuerySet
    # @api private
    #
    # Interprets a compiled {Planner::LevelPlan} against one data document.
    #
    # Every AST node evaluation increments `visits` and every projected /
    # flattened element increments an iteration counter, so fused and separate
    # runs are measured with the very same ruler.
    #
    # Error isolation: each fused *group* is guarded with the query ids that
    # own it. A shared group fails every query flowing through it (matching
    # separate semantics), while a divergent single-entry group fails only
    # that query. Inside a projection a failed member is skipped for the rest
    # of the traversal.
    class Executor
      # Access counters for one execution.
      Stats = Struct.new(:visits, :array_iterations, :object_iterations) do
        def total_iterations
          array_iterations + object_iterations
        end

        def add!(other)
          self.visits += other.visits
          self.array_iterations += other.array_iterations
          self.object_iterations += other.object_iterations
          self
        end
      end

      attr_reader :stats, :errors

      def initialize(on_error:)
        @on_error = on_error
        @stats = Stats.new(0, 0, 0)
        @errors = {}
      end

      def run(plan, input)
        eval_level(plan, input)
      end

      private

      # Evaluate one fused level; every entry shares `input`.
      def eval_level(plan, input)
        values = {}
        plan.groups.each do |group|
          result = guard(group.owners) { eval_group(group, input) }
          values.merge!(result) unless result == :missing
        end
        values
      end

      def eval_group(group, input)
        ids = group.entry_ids
        case group.type
        when :identity
          hash_for(ids, input)
        when :array_projection, :object_projection
          eval_projection(group, input, ids)
        when :condition
          eval_condition(group, input, ids)
        when :and, :or
          eval_gate(group, input, ids)
        else
          eval_passthrough(group, input, ids)
        end
      end

      def hash_for(ids, value)
        ids.each_with_object({}) { |id, h| h[id] = value }
      end

      # ---- shared internal children ---------------------------------------

      def eval_internals(group, input)
        level = group.internal_level
        return nil unless level

        values = eval_level(level, input)
        return :missing unless group.slot_ids.all? { |sid| values.key?(sid) }

        values
      end

      # ---- passthrough nodes ----------------------------------------------

      def eval_passthrough(group, input, ids)
        slot_values = eval_internals(group, input)
        return surviving(ids) if slot_values == :missing

        slots = group.slot_ids.map { |sid| slot_values[sid] }
        @stats.visits += 1
        head = eval_head(group.node, input, slots)
        tails = eval_level(group.next_level, head)
        select_values(tails, ids)
      end

      def select_values(values, ids)
        ids.each_with_object({}) { |id, h| h[id] = values[id] if values.key?(id) }
      end

      # ---- projections -----------------------------------------------------

      def eval_projection(group, input, ids)
        slot_values = eval_internals(group, input)
        return surviving(ids) if slot_values == :missing

        targets = extract_targets(group.type, slot_values[group.slot_ids.first])
        return gated_tails(group, ids, nil) unless targets

        collected = {}
        ids.each { |id| collected[id] = [] }

        if (fast = fast_branch(group, ids))
          run_fast_projection(targets, collected, fast)
        else
          branch_level = group.branch_level
          targets.each do |element|
            branch_vals = eval_level(branch_level, element)
            ids.each do |id|
              next if @errors.key?(id)

              v = branch_vals[id]
              collected[id] << v if branch_vals.key?(id) && !v.nil?
            end
          end
        end

        member_tails = group.member_tails
        result = {}
        ids.each do |id|
          next if @errors.key?(id)

          plan = member_tails[id]
          result[id] = trivial?(plan) ? collected[id] : eval_level(plan, collected[id])[id]
        end
        result
      end

      # ---- fast per-element branches --------------------------------------
      #
      # The overwhelmingly common projection shape is: walk the array once,
      # optionally test one shared predicate, then pull a few leaf fields
      # (possibly nested, possibly multi-selects) out of each element. These
      # descriptors let that loop run with no per-element hash maps.

      def fast_branch(group, _ids)
        branch = group.branch_level
        segments = []
        abort_flag = false
        branch.groups.each do |g|
          if g.type == :condition
            member_ids = []
            extractors = []
            g.entry_ids.each do |id|
              extractor = extractor_for_level(g.child_level, id)
              abort_flag = true unless extractor
              break if abort_flag

              member_ids << id
              extractors << extractor
            end
            break if abort_flag

            test_node = g.node.test
            segments << { kind: :condition,
                          member_ids: member_ids, extractors: extractors,
                          test_node: test_node, direct: direct_test?(test_node),
                          internal: g.internal_level, test_slot: g.slot_ids.first }
          else
            # plain (wildcard / flatten-continuation) branch entry
            member_ids = g.entry_ids
            if member_ids.size != 1
              abort_flag = true
              break
            end
            extractor = extractor_for_group(g)
            unless extractor
              abort_flag = true
              break
            end
            segments << { kind: :plain, member_ids: member_ids, extractors: [extractor] }
          end
        end

        abort_flag ? nil : { segments: segments }
      end

      # Compiles a single branch entry into a directly-runnable extractor:
      # a leaf chain, or a multi-select list/hash whose parts are leaves.
      def extractor_for_level(level, id)
        group = level.groups.find { |g| g.entry_ids == [id] }
        group && extractor_for_group(group)
      end

      def extractor_for_group(group)
        # Multi-select nodes carry their compiled parts as internals; pull
        # the descriptors straight from those leaf groups.
        if group.type == :multi_select_list && (group.next_level.nil? || identity_level?(group.next_level))
          parts = group.internal_level ? parts_from(group.internal_level) : group.node.children.map { |c| simple_extractor(c) }
          return parts.all? ? { kind: :msl, parts: parts } : nil
        elsif group.type == :multi_select_hash && (group.next_level.nil? || identity_level?(group.next_level))
          pairs = msh_pairs(group)
          return pairs && pairs.all? { |_, e| e } ? { kind: :msh, pairs: pairs } : nil
        end

        return nil unless group.internal_level.nil?

        head_extractor(group.node, group.next_level)
      end

      def parts_from(internal_level)
        parts = internal_level.groups.map do |g|
          break nil unless g.entry_ids.size == 1

          extractor_for_group_chain(g)
        end
        parts && parts.all? ? parts : nil
      end

      def msh_pairs(group)
        keys = group.node.kv_pairs.map(&:key)
        unless group.internal_level
          return keys.zip(group.node.kv_pairs.map { |p| simple_extractor(p.value) })
        end

        exs = parts_from(group.internal_level)
        exs ? keys.zip(exs) : nil
      end

      # A group whose head is a leaf and whose tail is an identity/leaf chain.
      def extractor_for_group_chain(group)
        return nil unless group.internal_level.nil?

        head_extractor(group.node, group.next_level)
      end

      def head_extractor(node, next_level)
        case AST.kind(node)
        when :leaf
          chain = leaf_chain(next_level)
          chain.nil? ? nil : { kind: :chain, nodes: [node, *chain] }
        when :multi_select_list
          return nil unless next_level.nil?

          parts = node.children.map { |c| simple_extractor(c) }
          parts.all? ? { kind: :msl, parts: parts } : nil
        when :multi_select_hash
          return nil unless next_level.nil?

          pairs = node.kv_pairs.map { |p| [p.key, simple_extractor(p.value)] }
          pairs.all? { |_, e| e } ? { kind: :msh, pairs: pairs } : nil
        else nil
        end
      end

      def simple_extractor(node)
        case AST.kind(node)
        when :leaf then { kind: :chain, nodes: [node] }
        when :multi_select_list
          parts = node.children.map { |c| simple_extractor(c) }
          parts.all? ? { kind: :msl, parts: parts } : nil
        when :multi_select_hash
          pairs = node.kv_pairs.map { |p| [p.key, simple_extractor(p.value)] }
          pairs.all? { |_, e| e } ? { kind: :msh, pairs: pairs } : nil
        else nil
        end
      end

      # A next_level made solely of single-entry leaf groups is a field chain.
      def leaf_chain(level)
        return [] if level.nil?
        return [] if identity_level?(level)
        return nil unless level.groups.size == 1

        g = level.groups.first
        return nil unless g.entry_ids.size == 1 && AST.kind(g.node) == :leaf && g.internal_level.nil?

        if g.next_level.nil? || identity_level?(g.next_level)
          [g.node]
        elsif (rest = leaf_chain(g.next_level))
          [g.node, *rest]
        end
      end

      def identity_level?(level)
        level.groups.all? { |g| g.type == :identity && g.next_level.nil? }
      end

      def trivial?(level)
        level.nil? || identity_level?(level)
      end

      def run_fast_projection(targets, collected, fast)
        segments = fast[:segments]
        targets.each do |element|
          segments.each do |segment|
            if segment[:kind] == :condition
              passed =
                if segment[:direct]
                  next if segment_dead?(segment)

                  r = guard(segment[:member_ids]) { run_direct_test(segment[:test_node], element) }
                  next if r == :missing

                  r
                else
                  test_vals = eval_level(segment[:internal], element)
                  next unless test_vals.key?(segment[:test_slot])

                  !Util.falsey?(test_vals[segment[:test_slot]])
                end
              emit_segment(segment, element, collected) if passed
            else
              emit_segment(segment, element, collected)
            end
          end
        end
      end

      def segment_dead?(segment)
        segment[:member_ids].all? { |id| @errors.key?(id) }
      end

      def emit_segment(segment, element, collected)
        ids = segment[:member_ids]
        extractors = segment[:extractors]
        i = 0
        while i < ids.size
          id = ids[i]
          unless @errors.key?(id)
            v = guard([id]) { run_extractor(extractors[i], element) }
            collected[id] << v unless v.nil? || v == :missing
          end
          i += 1
        end
      end

      def run_extractor(extractor, value)
        case extractor[:kind]
        when :chain
          @stats.visits += extractor[:nodes].size
          extractor[:nodes].each do |node|
            value = node.visit(value)
          end
          value
        when :msl
          @stats.visits += 1
          return nil if value.nil?

          extractor[:parts].map { |part| run_extractor(part, value) }
        when :msh
          @stats.visits += 1
          return nil if value.nil?

          extractor[:pairs].each_with_object({}) { |(key, part), h| h[key] = run_extractor(part, value) }
        end
      end

      def direct_test?(node)
        case AST.kind(node)
        when :leaf then true
        when :comparator then simple_leaf?(node.left) && simple_leaf?(node.right)
        else false
        end
      end

      def simple_leaf?(node)
        AST.kind(node) == :leaf
      end

      def run_direct_test(node, element)
        if AST.kind(node) == :comparator
          @stats.visits += 3 # comparator plus its two operands
          lv = node.left.visit(element)
          rv = node.right.visit(element)
          !Util.falsey?(node.check(lv, rv))
        else
          @stats.visits += 1
          !Util.falsey?(node.visit(element))
        end
      end

      # ---- conditions / and / or (non-projection contexts) ----------------

      def eval_condition(group, input, ids)
        slot_values = eval_internals(group, input)
        return surviving(ids) if slot_values == :missing

        if Util.falsey?(slot_values[group.slot_ids.first])
          return gated_tails(group, ids, nil)
        end

        child_vals = eval_level(group.child_level, input)
        active = ids.reject { |id| @errors.key?(id) || !child_vals.key?(id) }
        continue_tails(group, active, child_vals)
      end

      def eval_gate(group, input, ids)
        slot_values = eval_internals(group, input)
        return surviving(ids) if slot_values == :missing

        left_value = slot_values[group.slot_ids.first]
        take_arm = group.type == :and ? !Util.falsey?(left_value) : Util.falsey?(left_value)

        if take_arm
          arm_vals = eval_level(group.arm_level, input)
          active = ids.reject { |id| @errors.key?(id) || !arm_vals.key?(id) }
          continue_tails(group, active, arm_vals)
        else
          values = ids.each_with_object({}) { |id, h| h[id] = left_value unless @errors.key?(id) }
          continue_tails(group, values.keys, values)
        end
      end

      def gated_tails(group, ids, shared)
        values = ids.each_with_object({}) { |id, h| h[id] = shared unless @errors.key?(id) }
        continue_tails(group, values.keys, values)
      end

      def continue_tails(group, ids, values)
        member_tails = group.member_tails
        result = {}
        ids.each do |id|
          next if @errors.key?(id)

          plan = member_tails[id]
          result[id] = trivial?(plan) ? values[id] : eval_level(plan, values[id])[id]
        end
        result
      end

      # ---- head evaluation -------------------------------------------------

      def eval_head(node, input, slots)
        case AST.kind(node)
        when :array_projection, :object_projection
          project_once(node, slots.first)
        when :flatten
          flatten_once(slots.first)
        when :condition
          Util.falsey?(slots.first) ? nil : input
        when :comparator
          node.check(slots[0], slots[1])
        when :not
          Util.falsey?(slots.first)
        when :and, :or then slots.first
        when :function
          node.call(slots)
        when :multi_select_hash
          return nil if input.nil?

          node.kv_pairs.each_with_index.each_with_object({}) { |(pair, i), h| h[pair.key] = slots[i] }
        when :multi_select_list
          input.nil? ? input : slots
        when :expression
          node
        else
          node.visit(input)
        end
      end

      def project_once(node, target_value)
        targets = extract_targets(AST.kind(node), target_value)
        return nil unless targets

        @stats.visits += 1
        list = []
        targets.each do |v|
          vv = node.projection.visit(v)
          list << vv unless vv.nil?
        end
        list
      end

      def flatten_once(value)
        return unless value.respond_to?(:to_ary)

        arr = value.to_ary
        @stats.array_iterations += arr.size
        arr.each_with_object([]) do |v, values|
          if v.respond_to?(:to_ary)
            values.concat(v.to_ary)
          else
            values.push(v)
          end
        end
      end

      def extract_targets(type, target)
        case type
        when :object_projection
          if target.respond_to?(:to_hash)
            @stats.object_iterations += target.to_hash.size
            target.to_hash.values
          elsif target.is_a?(Struct)
            @stats.object_iterations += target.members.size
            target.values
          end
        else
          if target.respond_to?(:to_ary)
            @stats.array_iterations += target.to_ary.size
            target.to_ary
          end
        end
      end

      # ---- guards / errors -------------------------------------------------

      def surviving(ids)
        ids.each_with_object({}) { |id, h| h[id] = nil unless @errors.key?(id) }
      end

      def guard(qids)
        yield
      rescue StandardError => e
        alive = Array(qids).uniq.reject { |q| @errors.key?(q) }
        raise e if @on_error == :raise

        alive.each { |q| @errors[q] = e }
        :missing
      end
    end
  end
end