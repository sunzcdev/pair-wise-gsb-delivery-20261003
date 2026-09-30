 # frozen_string_literal: true
module JMESPath
  module Sourced
    # @api private
    #
    # Evaluates an optimized JMESPath AST while carrying JSON Pointer
    # provenance for every produced value.
    class Interpreter
      def initialize(node)
        @node = node
      end

      def evaluate(data)
        root = Located.sourced(data, '')
        result = visit(@node, root)
        build_result(result)
      end

      # Public entry point used by expression references such as the
      # argument of `sort_by()`, `map()` and `*_by()` functions.
      def resolve(node, located)
        visit(node, located)
      end

      private

      def visit(node, located)
        case node
        when Nodes::Current then located
        when Nodes::ChainedField then visit_chained_field(node, located)
        when Nodes::Field then visit_field(node, located)
        when Nodes::Literal then visit_literal(node)
        when Nodes::Chain then visit_chain(node, located)
        when Nodes::Subexpression then visit_chain_children([node.left, node.right], located)
        when Nodes::FastArrayProjection, Nodes::FastObjectProjection
          visit_projection(node, located, fast: true)
        when Nodes::ArrayProjection, Nodes::ObjectProjection
          visit_projection(node, located, fast: false)
        when Nodes::Flatten then visit_flatten(node, located)
        when Nodes::SimpleSlice then visit_slice(node, located)
        when Nodes::Slice then visit_slice(node, located)
        when Nodes::MultiSelectHash then visit_multi_select_hash(node, located)
        when Nodes::MultiSelectList then visit_multi_select_list(node, located)
        when Nodes::Condition then visit_condition(node, located)
        when Nodes::ComparatorCondition then visit_comparator_condition(node, located)
        when Nodes::Comparator then visit_comparator(node, located)
        when Nodes::And then visit_and(node, located)
        when Nodes::Or then visit_or(node, located)
        when Nodes::Not then visit_not(node, located)
        when Nodes::Expression then Located.derived(node, [])
        when Nodes::Function then visit_function(node, located)
        else
          raise Errors::RuntimeError, "unsupported node #{node.class}"
        end
      end

      # ----------------------------------------------------------------
      # navigation
      # ----------------------------------------------------------------

      def visit_chain(node, located)
        node.children.reduce(located) { |current, child| visit(child, current) }
      end

      def visit_chain_children(children, located)
        children.reduce(located) do |current, child|
          if child.is_a?(Nodes::Subexpression)
            visit_chain_children([child.left, child.right], current)
          else
            visit(child, current)
          end
        end
      end

      def visit_chained_field(node, located)
        node.keys.reduce(located) do |current, key|
          current.missing? ? current : locate_field(current, key)
        end
      end

      def visit_field(node, located)
        located.missing? ? located : locate_field(located, node.key)
      end

      def locate_field(located, key)
        return Located::MISSING if located.missing?

        if located.synthetic?
          locate_synthetic(located, key)
        elsif key.is_a?(Integer) && located.raw.respond_to?(:to_ary)
          locate_index(located, key)
        elsif located.raw.respond_to?(:to_hash)
          locate_hash_key(located, key)
        elsif located.raw.is_a?(Struct)
          locate_struct_member(located, key)
        else
          Located::MISSING
        end
      end

      def locate_index(located, index)
        array = located.raw.to_ary
        size = array.size
        normalized = index < 0 ? index + size : index
        return Located::MISSING unless normalized >= 0 && normalized < size

        child_for(located, array, normalized)
      end

      def locate_hash_key(located, key)
        hash = located.raw.to_hash
        token =
          if hash.key?(key)
            key
          elsif key.respond_to?(:to_sym) && hash.key?(key.to_sym)
            key.to_sym
          end
        return Located::MISSING if token.nil?

        child_for(located, hash, token)
      end

      def locate_struct_member(located, key)
        struct = located.raw
        return Located::MISSING unless struct.members.map(&:to_s).include?(key.to_s)

        member = struct.members.find { |m| m.to_s == key.to_s }
        child_for(located, struct, member)
      end

      def locate_synthetic(located, key)
        raw = located.raw
        if key.is_a?(Integer) && raw.respond_to?(:to_ary)
          array = raw.to_ary
          normalized = key < 0 ? key + array.size : key
          return Located::MISSING unless normalized >= 0 && normalized < array.size

          found = located.children.find { |token, _| token == normalized }
          return found.last if found

          return Located.derived(array[normalized], located.root_inputs)
        end

        found = located.children.find do |token, child|
          token == key || (token.respond_to?(:to_s) && key.respond_to?(:to_s) && token.to_s == key.to_s)
        end
        return found.last if found

        if !key.is_a?(Integer) && raw.respond_to?(:to_hash)
          hash = raw.to_hash
          token = hash.key?(key) ? key : (key.respond_to?(:to_sym) && hash.key?(key.to_sym) ? key.to_sym : nil)
          return Located::MISSING if token.nil?

          Located.derived(hash[token], located.root_inputs)
        end
        Located::MISSING
      end

      # ----------------------------------------------------------------
      # literals
      # ----------------------------------------------------------------

      def visit_literal(node)
        Located.derived(node.value, [])
      end

      # ----------------------------------------------------------------
      # projections, slices and flatten
      # ----------------------------------------------------------------

      def visit_projection(node, located, fast:)
        target = visit(node.target, located)
        return Located::MISSING if target.missing?

        members = projection_members(node, target)
        return Located::MISSING unless members

        entries = []
        members.each do |token, child|
          projected =
            if fast
              child
            else
              visit(node.projection, child)
            end
          next if projected.missing? || projected.raw.nil?

          entries << [entries.size, projected]
        end

        Located.derived(entries.map { |_, e| e.raw }, union_inputs(entries.map(&:last)), entries)
      end

      def projection_members(node, target)
        if node.is_a?(Nodes::ArrayProjection)
          return nil unless target.raw.respond_to?(:to_ary)

          array = target.raw.to_ary
          array.each_index.map do |index|
            [index, child_for(target, array, index)]
          end
        else
          if target.raw.respond_to?(:to_hash)
            hash = target.raw.to_hash
            hash.each_key.map { |key| [key, child_for(target, hash, key)] }
          elsif target.raw.is_a?(Struct)
            struct = target.raw
            struct.members.map { |member| [member, child_for(target, struct, member)] }
          end
        end
      end

      def visit_flatten(node, located)
        child = visit(node.child, located)
        return Located::MISSING if child.missing?
        return Located::MISSING unless child.raw.respond_to?(:to_ary)

        array = child.raw.to_ary
        entries = []
        array.each_index do |index|
          element = child_for(child, array, index)
          next if element.missing?

          if element.raw.respond_to?(:to_ary)
            nested = element.raw.to_ary
            nested.each_index do |nested_index|
              sub = child_for(element, nested, nested_index)
              next if sub.missing?

              entries << [entries.size, sub]
            end
          else
            entries << [entries.size, element]
          end
        end
        Located.derived(entries.map { |_, e| e.raw }, union_inputs(entries.map(&:last)), entries)
      end

      def visit_slice(node, located)
        return Located::MISSING if located.missing?

        return Located::MISSING unless located.raw.respond_to?(:to_ary)

        raw = located.raw.to_ary

        indices = slice_indices(raw.size, node.start, node.stop, node.step)
        entries = []
        indices.each do |source_index|
          entries << [entries.size, child_for(located, raw, source_index)]
        end
        Located.derived(indices.map { |i| raw[i] }, union_inputs(entries.map(&:last)), entries)
      end

      def slice_indices(length, raw_start, raw_stop, raw_step)
        step = raw_step || 1
        start =
          if raw_start.nil?
            step < 0 ? length - 1 : 0
          else
            adjust_endpoint(length, raw_start, step)
          end
        stop =
          if raw_stop.nil?
            step < 0 ? -1 : length
          else
            adjust_endpoint(length, raw_stop, step)
          end

        indices = []
        if step > 0
          i = start
          while i < stop
            indices << i
            i += step
          end
        else
          i = start
          while i > stop
            indices << i
            i += step
          end
        end
        indices
      end

      def adjust_endpoint(length, endpoint, step)
        if endpoint < 0
          endpoint += length
          endpoint = step < 0 ? -1 : 0 if endpoint < 0
          endpoint
        elsif endpoint >= length
          step < 0 ? length - 1 : length
        else
          endpoint
        end
      end

      # ----------------------------------------------------------------
      # multi-selects
      # ----------------------------------------------------------------

      def visit_multi_select_hash(node, located)
        return Located::MISSING if located.missing? || located.raw.nil?

        entries = []
        raw = {}
        node.kv_pairs.each do |pair|
          child = visit(pair.value, located)
          raw[pair.key] = child.missing? ? nil : child.raw
          entries << [pair.key, child.missing? ? Located.derived(nil, located.root_inputs) : child]
        end
        Located.derived(raw, union_inputs(entries.map(&:last)), entries)
      end

      def visit_multi_select_list(node, located)
        return Located::MISSING if located.missing? || located.raw.nil?

        entries = []
        node.children.each_with_index do |child_node, index|
          child = visit(child_node, located)
          child = Located.derived(nil, located.root_inputs) if child.missing?
          entries << [index, child]
        end
        Located.derived(entries.map { |_, child| child.raw }, union_inputs(entries.map(&:last)), entries)
      end

      # ----------------------------------------------------------------
      # conditions and comparisons
      # ----------------------------------------------------------------

      def visit_condition(node, located)
        test = visit(node.test, located)
        if test.missing? || Util.falsey?(test.raw)
          Located::MISSING
        else
          visit(node.child, located)
        end
      end

      def visit_comparator_condition(node, located)
        left = visit(node.left, located)
        right =
          if node.is_a?(Nodes::LiteralRightEqCondition) || node.is_a?(Nodes::LiteralRightNeqCondition)
            Located.derived(node.right, [])
          else
            visit(node.right, located)
          end

        if comparator_true?(node, left, right)
          visit(node.child, located)
        else
          Located::MISSING
        end
      end

      def comparator_true?(node, left, right)
        left_value = Util.as_json(left.missing? ? nil : left.raw)
        right_value = Util.as_json(right.missing? ? nil : right.raw)

        case node
        when Nodes::LiteralRightEqCondition, Nodes::EqCondition
          left_value == right_value
        when Nodes::LiteralRightNeqCondition, Nodes::NeqCondition
          left_value != right_value
        when Nodes::GtCondition
          comparable?(left_value, right_value) && left_value > right_value
        when Nodes::GteCondition
          comparable?(left_value, right_value) && left_value >= right_value
        when Nodes::LtCondition
          comparable?(left_value, right_value) && left_value < right_value
        when Nodes::LteCondition
          comparable?(left_value, right_value) && left_value <= right_value
        else
          false
        end
      end

      def comparable?(left_value, right_value)
        Nodes::Comparator::COMPARABLE_TYPES.any? do |type|
          left_value.is_a?(type) && right_value.is_a?(type)
        end
      end

      def visit_comparator(node, located)
        left = visit(node.left, located)
        right = visit(node.right, located)
        raw = node.visit(located.raw)
        inputs = (left.root_inputs + right.root_inputs).uniq
        Located.derived(raw, inputs)
      end

      def visit_and(node, located)
        left = visit(node.left, located)
        if left.missing? || Util.falsey?(left.raw)
          left
        else
          visit(node.right, located)
        end
      end

      def visit_or(node, located)
        left = visit(node.left, located)
        if left.missing? || Util.falsey?(left.raw)
          visit(node.right, located)
        else
          left
        end
      end

      def visit_not(node, located)
        operand = visit(node.expression, located)
        Located.derived(Util.falsey?(operand.missing? ? nil : operand.raw), operand.root_inputs)
      end

      # ----------------------------------------------------------------
      # built-in functions
      # ----------------------------------------------------------------

      def visit_function(node, located)
        args = node.children.map { |child| visit(child, located) }

        case node
        when Nodes::SortFunction then function_sort(node, args)
        when Nodes::SortByFunction then function_sort_by(node, args)
        when Nodes::ReverseFunction then function_reverse(node, args)
        when Nodes::Map then function_map(args)
        when Nodes::MaxFunction then function_pick_extreme(node, args, :max)
        when Nodes::MinFunction then function_pick_extreme(node, args, :min)
        when Nodes::MaxByFunction then function_pick_by(node, args, :max_by)
        when Nodes::MinByFunction then function_pick_by(node, args, :min_by)
        when Nodes::KeysFunction then function_keys(args)
        when Nodes::ValuesFunction then function_values(args)
        when Nodes::MergeFunction then function_merge(args)
        when Nodes::ToArrayFunction then function_to_array(args)
        when Nodes::NotNullFunction then function_not_null(args)
        when Nodes::JoinFunction then function_join(node, args)
        when Nodes::SumFunction, Nodes::AvgFunction then function_numeric_aggregate(node, args)
        else
          generic_function(node, args)
        end
      end

      def function_sort(node, args)
        values = args.first
        return Located.derived(nil, []) unless values.raw.respond_to?(:to_ary)

        array = values.raw.to_ary
        raw = Nodes::SortFunction.new([], disable_visit_errors: node.disable_visit_errors).send(:call, [array])
        return Located.derived(raw, values.root_inputs) if raw.nil? && !array.empty?
        entries = []
        order = stable_order(array.size) { |i| array[i] }
        order.each do |source_index|
          entries << [entries.size, child_for(values, array, source_index)]
        end
        Located.derived(order.map { |i| array[i] }, values.root_inputs, entries)
      end

      def function_sort_by(node, args)
        values, expression = args
        return Located.derived(nil, []) unless values.raw.respond_to?(:to_ary) && expression.raw.is_a?(Nodes::Expression)

        array = values.raw.to_ary
        keys = array.each_index.map do |index|
          evaluated = expression.raw.eval_located(self, child_for(values, array, index))
          evaluated.missing? ? nil : evaluated.raw
        end
        raw = Nodes::SortByFunction.new([], disable_visit_errors: node.disable_visit_errors)
                                           .send(:call, [array, expression.raw])
        return Located.derived(raw, values.root_inputs) if raw.nil? && !array.empty?
        order = stable_order(array.size) { |i| keys[i] }
        entries = []
        order.each do |source_index|
          entries << [entries.size, child_for(values, array, source_index)]
        end
        Located.derived(order.map { |i| array[i] }, values.root_inputs, entries)
      end

      def function_reverse(node, args)
        values = args.first
        raw = Nodes::ReverseFunction.new([], disable_visit_errors: node.disable_visit_errors).send(:call, [values.raw])
        if values.raw.respond_to?(:to_str)
          Located.derived(raw, values.root_inputs)
        elsif values.raw.respond_to?(:to_ary)
          array = values.raw.to_ary
          entries = []
          (array.size - 1).downto(0) do |source_index|
            entries << [entries.size, child_for(values, array, source_index)]
          end
          Located.derived(raw, values.root_inputs, entries)
        else
          Located.derived(raw, [])
        end
      end

      def function_map(args)
        expression, values = args
        return Located.derived(nil, []) unless values.raw.respond_to?(:to_ary)

        array = values.raw.to_ary
        entries = []
        array.each_index do |index|
          child = expression.raw.eval_located(self, child_for(values, array, index))
          entries << [entries.size, child]
        end
        Located.derived(entries.map { |_, child| child.raw }, union_inputs(entries.map(&:last)), entries)
      end

      def function_pick_extreme(node, args, mode)
        values = args.first
        array = values.raw.respond_to?(:to_ary) ? values.raw.to_ary : nil
        raw = node.send(:call, [values.raw])
        return Located.derived(raw, values.root_inputs) unless array
        return Located.derived(nil, values.root_inputs) if array.empty? || raw.nil?

        source_index = array.index { |entry| entry.equal?(raw) }
        source_index ||= array.index(raw)
        source_index ? child_for(values, array, source_index) : Located.derived(raw, values.root_inputs)
      end

      def function_pick_by(node, args, mode)
        values, expression = args
        raw = node.send(:call, [values.raw, expression.raw])
        unless values.raw.respond_to?(:to_ary) && expression.raw.is_a?(Nodes::Expression)
          return Located.derived(raw, values.root_inputs)
        end

        array = values.raw.to_ary
        return Located.derived(nil, values.root_inputs) if array.empty? || raw.nil?

        source_index = array.index { |entry| entry.equal?(raw) }
        source_index ||= array.index(raw)
        source_index ? child_for(values, array, source_index) : Located.derived(raw, values.root_inputs)
      end

      def function_keys(args)
        value = args.first
        raw_keys =
          if value.raw.respond_to?(:to_hash)
            value.raw.to_hash.keys.map(&:to_s)
          elsif value.raw.is_a?(Struct)
            value.raw.members.map(&:to_s)
          else
            return Located.derived(nil, [])
          end
        Located.derived(raw_keys, value.root_inputs)
      end

      def function_values(args)
        value = args.first
        if value.raw.respond_to?(:to_hash)
          hash = value.raw.to_hash
          entries = hash.each_key.map { |key| [key, child_for(value, hash, key)] }
          entries = entries.each_with_index.map { |entry, index| [index, entry.last] }
          Located.derived(hash.values, value.root_inputs, entries)
        elsif value.raw.is_a?(Struct)
          struct = value.raw
          entries = struct.members.each_with_index.map do |member, index|
            [index, child_for(value, struct, member)]
          end
          Located.derived(struct.values, value.root_inputs, entries)
        elsif value.raw.respond_to?(:to_ary)
          array = value.raw.to_ary
          entries = array.each_index.map { |index| [index, child_for(value, array, index)] }
          Located.derived(array, value.root_inputs, entries)
        else
          Located.derived(nil, [])
        end
      end

      def function_merge(args)
        entries = []
        merged = {}
        args.each do |arg|
          next unless arg.raw.respond_to?(:to_hash)

          arg.raw.to_hash.each_pair do |key, raw_value|
            merged[key] = raw_value
            existing = entries.find { |token, _| token == key }
            entries.delete(existing) if existing
            entries << [key, child_for(arg, arg.raw.to_hash, key)]
          end
        end
        Located.derived(merged, union_inputs(args), entries)
      end

      def function_to_array(args)
        value = args.first
        if value.raw.respond_to?(:to_ary)
          array = value.raw.to_ary
          entries = array.each_index.map { |index| [index, child_for(value, array, index)] }
          Located.derived(array, value.root_inputs, entries)
        else
          Located.derived([value.raw], value.root_inputs, [[0, value]])
        end
      end

      def function_not_null(args)
        found = args.find { |arg| !arg.missing? && !arg.raw.nil? }
        found || Located.derived(nil, [])
      end

      def function_join(node, args)
        glue, values = args
        values_raw = values.raw.respond_to?(:to_ary) ? values.raw.to_ary : values.raw
        raw = node.send(:call, [glue.raw, values_raw])
        Located.derived(raw, union_inputs(args))
      end

      def function_numeric_aggregate(node, args)
        raw = node.send(:call, args.map(&:raw))
        values = args.first
        inputs =
          if values.raw.respond_to?(:to_ary)
            array = values.raw.to_ary
            array.each_index.map { |index| child_for(values, array, index) }.flat_map(&:root_inputs)
          else
            values.root_inputs
          end
        Located.derived(raw, inputs.uniq)
      end

      def generic_function(node, args)
        raw = node.send(:call, args.map(&:raw))
        Located.derived(raw, union_inputs(args))
      end

      def stable_order(size)
        n = 0
        (0...size).to_a.sort_by do |index|
          n += 1
          [yield(index), n]
        end
      end

      # ----------------------------------------------------------------
      # container traversal
      # ----------------------------------------------------------------

      # Resolves the Located entry at position `token` of the raw container
      # `container` reachable through `located`. Synthetic containers hold
      # explicit Located entries; verbatim containers point back into the
      # document by position; derived containers inherit the parent inputs.
      def child_for(located, container, token)
        if located.synthetic?
          pair = located.children.find { |child_token, _| child_token == token }
          pair ? pair.last : Located.derived(container_value(container, token), located.root_inputs)
        elsif located.sourced?
          Located.sourced(container_value(container, token), join_pointer(located.pointer, token))
        else
          Located.derived(container_value(container, token), located.root_inputs)
        end
      end

      def container_value(container, token)
        if container.is_a?(Struct)
          container[token]
        else
          container[token]
        end
      end

      def join_pointer(pointer, token)
        escaped = token.to_s.gsub('~', '~0').gsub('/', '~1')
        "#{pointer}/#{escaped}"
      end

      def union_inputs(located_values)
        located_values.flat_map do |located|
          located.missing? ? [] : located.root_inputs
        end.uniq
      end

      # ----------------------------------------------------------------
      # result construction
      # ----------------------------------------------------------------

      def build_result(located)
        locations = {}
        value = emit(located, '', locations)
        SourcedResult.new(value, locations)
      end

      def emit(located, pointer, locations)
        if located.missing?
          locations[pointer] = SourceLocation.missing
          return nil
        end

        if located.sourced?
          locations[pointer] = SourceLocation.sourced(located.pointer)
          emit_verbatim_children(located, pointer, locations)
        else
          locations[pointer] = SourceLocation.derived(located.inputs || [])
          if located.synthetic?
            emit_synthetic_children(located, pointer, locations)
          elsif located.raw.respond_to?(:to_hash)
            emit_derived_children(located, pointer, locations, :hash)
          elsif located.raw.respond_to?(:to_ary)
            emit_derived_children(located, pointer, locations, :array)
          end
        end
        located.raw
      end

      def emit_verbatim_children(located, pointer, locations)
        raw = located.raw
        if raw.respond_to?(:to_hash)
          raw.to_hash.each_key do |key|
            child = Located.sourced(raw.to_hash[key], join_pointer(located.pointer, key))
            emit(child, join_pointer(pointer, key), locations)
          end
        elsif raw.respond_to?(:to_ary)
          raw.to_ary.each_index do |index|
            child = Located.sourced(raw.to_ary[index], join_pointer(located.pointer, index))
            emit(child, join_pointer(pointer, index), locations)
          end
        elsif raw.is_a?(Struct)
          raw.members.each do |member|
            child = Located.sourced(raw[member], join_pointer(located.pointer, member))
            emit(child, join_pointer(pointer, member), locations)
          end
        end
      end

      def emit_synthetic_children(located, pointer, locations)
        located.children.each do |token, child|
          emit(child, join_pointer(pointer, token), locations)
        end
      end

      def emit_derived_children(located, pointer, locations, kind)
        raw = located.raw
        tokens = kind == :hash ? raw.to_hash.keys : (0...raw.to_ary.size).to_a
        tokens.each do |token|
          value = kind == :hash ? raw.to_hash[token] : raw.to_ary[token]
          child = Located.derived(value, located.inputs || [])
          emit(child, join_pointer(pointer, token), locations)
        end
      end
    end
  end
end
