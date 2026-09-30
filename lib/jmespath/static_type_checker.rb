# frozen_string_literal: true

require 'jmespath/shape'

module JMESPath
  # Compile-time shape checker and output-shape inferrer.
  #
  # The checker walks the *raw* AST produced by {Parser} (not the optimized
  # tree used for evaluation) and simulates what each node would do to a
  # value of a given {Shape}. It only reports an error when every possible
  # concrete input shape would fail at runtime. Anything that could still
  # succeed - open objects, +any+ from unknown data, heterogeneous unions -
  # is accepted and, when relevant, recorded as a runtime check.
  #
  #   result = JMESPath.check('abs(name)', people_shape)
  #   result.ok?
  #   result.errors.first.message
  #   result.output_shape
  class StaticTypeChecker
    # One piece of feedback about the expression.
    #
    # @!attribute severity :error+ (definite runtime failure) or +:runtime+
    #   (cannot be proven statically, the runtime check is kept).
    Finding = Struct.new(:severity, :node_class, :trail, :message, :reasons, keyword_init: true) do
      def error?
        severity == :error
      end

      def to_s
        location = trail.empty? ? node_label(node_class) : trail.join(' -> ')
        kind = severity == :error ? 'error' : 'runtime check'
        "#{kind} at #{location}: #{message}"
      end

      private

      def node_label(klass)
        klass.name.split('::').last.gsub(/([a-z\d])([A-Z])/, '\1 \2')
      end
    end

    # @!attribute output_shape [Shape] inferred shape of the query result
    # @!attribute findings [Array<Finding>]
    Result = Struct.new(:output_shape, :findings, keyword_init: true) do
      def errors
        findings.select(&:error?)
      end

      def runtime_checks
        findings.reject(&:error?)
      end

      def ok?
        errors.empty?
      end

      def to_s
        ok? ? "ok (#{runtime_checks.size} runtime check(s) deferred)" : "#{errors.size} compile-time error(s)"
      end
    end

    def initialize(input_shape, options = {})
      @input = Shape.coerce(input_shape)
      @parser = options[:parser] || Parser.new
      @findings = []
    end

    # @param [String] expression
    # @return [Result]
    def check(expression)
      ast = @parser.parse(expression)
      output = infer(ast, @input, [])
      Result.new(output_shape: output, findings: @findings.dup)
    end

    private

    # ==================================================================
    # Shape-directed AST simulation
    # ==================================================================

    def infer(node, shape, trail)
      case node
      when Nodes::Current
        shape
      when Nodes::Literal
        Shape.from_value(node.value)
      when Nodes::Field
        key = node.instance_variable_get(:@key)
        key.is_a?(Integer) ? infer_index(node, shape, trail) : infer_field(node, shape, trail)
      when Nodes::Subexpression
        left_node = node.instance_variable_get(:@left)
        right_node = node.instance_variable_get(:@right)
        right_shape = infer(left_node, shape, trail + ['left'])
        infer(right_node, right_shape, trail + ['right'])
      when Nodes::MultiSelectHash
        infer_multi_select_hash(node, shape, trail)
      when Nodes::MultiSelectList
        children = node.instance_variable_get(:@children)
        children.each_with_index.map do |child, index|
          infer(child, shape, trail + ["[#{index}]"])
        end.then { |shapes| Shape.array(Shape.union(shapes)) }
      when Nodes::ArrayProjection
        infer_array_projection(node, shape, trail)
      when Nodes::ObjectProjection
        infer_object_projection(node, shape, trail)
      when Nodes::Flatten
        target = infer(node.instance_variable_get(:@child), shape, trail + ['target'])
        flatten_shape(target)
      when Nodes::Slice
        infer_slice(node, shape, trail)
      when Nodes::Condition
        infer_condition(node, shape, trail)
      when Nodes::Comparator
        infer(node.left, shape, trail + ['left'])
        infer(node.right, shape, trail + ['right'])
        Shape.boolean
      when Nodes::And
        infer_and(node, shape, trail)
      when Nodes::Or
        left_node = node.instance_variable_get(:@left)
        right_node = node.instance_variable_get(:@right)
        infer(left_node, shape, trail + ['left'])
        infer(right_node, shape, trail + ['right'])
        Shape.union(infer(left_node, shape, trail + ['left']),
                    infer(right_node, shape, trail + ['right']))
      when Nodes::Not
        infer(node.instance_variable_get(:@expression), shape, trail + ['expr'])
        Shape.boolean
      when Nodes::Expression
        Shape.expression
      when Nodes::Function
        infer_function(node, shape, trail)
      else
        Shape.any
      end
    end

    def infer_field(node, shape, trail)
      key = node.instance_variable_get(:@key)
      result = shape.lookup(key)
      if result.nil?
        # Non-object, non-indexable scalar handled by the shape layer; this
        # branch only covers genuinely unsupported shapes.
        Shape.null
      else
        if shape.object? && shape.open? && !shape.fields.key?(key.to_s)
          note(node, trail,
               "field `#{key}` is not declared in the object shape",
               ['object is open, so the key may exist at runtime',
                'access yields `any | null`; the runtime lookup is kept'])
        end
        result
      end
    end

    def infer_index(node, shape, trail)
      index = node.instance_variable_get(:@key)
      result =
        if shape.array?
          shape.element.add_null
        elsif shape.any?
          Shape.nullable(Shape.any)
        elsif shape.union?
          indexed = shape.members.map do |member|
            if member.array?
              member.element.add_null
            elsif member.any?
              Shape.nullable(Shape.any)
            else
              Shape.null
            end
          end
          Shape.union(indexed)
        else
          Shape.null
        end

      note(node, trail,
           "index [#{index}] bounds are unknown",
           ['array length is not part of the shape',
            'result is the element type or null; bounds stay a runtime check'])
      result
    end

    def infer_multi_select_hash(node, shape, trail)
      fields = {}
      node.instance_variable_get(:@kv_pairs).each do |pair|
        value_shape = infer(pair.value, shape, trail + ["{#{pair.key}}"])
        fields[pair.key.to_s] = Shape::Field.new(value_shape, required: true)
      end
      result = Shape.object(fields, open: false)
      shape.nullable? ? result.add_null : result
    end

    def infer_and(node, shape, trail)
      left_shape = infer(node.instance_variable_get(:@left), shape, trail + ['left'])
      right_shape = infer(node.instance_variable_get(:@right), shape, trail + ['right'])
      left_shape.nullable? ? Shape.union(Shape.null, right_shape) : right_shape
    end

    def infer_array_projection(node, shape, trail)
      target_node = node.instance_variable_get(:@target)
      target = infer(target_node, shape, trail + ['target'])

      projection = node.instance_variable_get(:@projection)

      if target_node.is_a?(Nodes::Flatten)
        infer_flatten_projection(target, projection, trail)
      else
        element = array_elements(target)
        return array_output(target, nil) if element.nil?

        projected = infer_projection_rhs(projection, element, trail + ['[]'])
        array_output(target, projected)
      end
    end

    # A flatten node already returns an array (its flattened elements). The
    # projection RHS is evaluated per flattened element and re-collected.
    def infer_flatten_projection(flattened, projection, trail)
      if flattened.null_kinding? && flattened.members.size == 1
        Shape.null
      else
        projected = infer_projection_rhs(projection, flattened, trail + ['[]'])
        projected.array? ? projected : Shape.array(projected.remove_null)
      end
    end

    # Element shape reachable when the target is an array. Returns nil when
    # the target is definitely not an array.
    def array_elements(target)
      return Shape.never if target.never?
      return target.element if target.array?
      return Shape.any if target.any?

      if target.union? && target.members.all? { |member| array_or_nullish?(member) }
        elements = target.members.map { |member| member.array? ? member.element : Shape.any }
        Shape.union(elements)
      end
    end

    # Wrap a per-element chain result back into an array, matching the
    # projection target (nullable when the target might not be an array).
    def array_output(target, projected)
      result =
        if projected.nil?
          Shape.array(Shape.never)
        else
          projected.array? ? projected : Shape.array(projected.remove_null)
        end
      definitely_array?(target) ? result : result.add_null
    end

    def definitely_array?(target)
      target.array? ||
        (target.union? && target.members.all?(&:array?))
    end

    def array_or_nullish?(member)
      member.array? || member.any? || member.null_kinding?
    end

    # The part of a query after a projection: a filter is applied per
    # element, then the surviving elements are collected into an array and
    # the remaining chain operates on that array.
    def infer_projection_rhs(node, element, trail)
      case node
      when Nodes::Condition
        narrowed = apply_filter(element, node, trail)
        infer_projection_child(node.instance_variable_get(:@child), narrowed, trail + ['branch'])
      when Nodes::Current
        element
      else
        infer_projection_child(node, element, trail)
      end
    end

    # The projection child runs its first link per element, collecting the
    # values into an array; any remaining chain operates on that array.
    def infer_projection_child(child, element, trail)
      return Shape.array(element) if child.is_a?(Nodes::Current)

      head, *tail = split_chain(child)
      first = infer_element_chain(head, element, trail)
      produced = first.array? ? first : Shape.array(first.remove_null)
      tail.empty? ? produced : infer_value_chain(join_chain(tail), produced, trail)
    end

    # Flatten a left-deep Subexpression chain into an ordered node list.
    def split_chain(node)
      nodes = []
      current = node
      while current.is_a?(Nodes::Subexpression)
        nodes << current.instance_variable_get(:@right)
        current = current.instance_variable_get(:@left)
      end
      [current] + nodes.reverse
    end

    # Rebuild the semantic composition of a chain tail (left-to-right).
    def join_chain(nodes)
      nodes.reduce { |left, right| Nodes::Subexpression.new(left, right) }
    end

    # Ordinary left-to-right evaluation on a produced value.
    def infer_value_chain(node, shape, trail)
      case node
      when Nodes::Subexpression
        middle = infer_value_chain(node.instance_variable_get(:@left), shape, trail + ['left'])
        infer_value_chain(node.instance_variable_get(:@right), middle, trail + ['right'])
      when Nodes::Pipe
        middle = infer_value_chain(node.instance_variable_get(:@left), shape, trail + ['left'])
        infer_value_chain(node.instance_variable_get(:@right), middle, trail + ['right'])
      else
        infer(node, shape, trail)
      end
    end

    # Chain applied to a single projected element. A nested array projection
    # switches back to array context for everything after it.
    def infer_element_chain(node, element, trail)
      case node
      when Nodes::Current
        element
      when Nodes::Field
        key = node.instance_variable_get(:@key)
        key.is_a?(Integer) ? infer_index(node, element, trail) : infer_field(node, element, trail)
      when Nodes::Subexpression
        middle = infer_element_chain(node.instance_variable_get(:@left), element, trail)
        continue_chain(node.instance_variable_get(:@right), middle, trail)
      when Nodes::ArrayProjection
        target = infer_element_chain(node.instance_variable_get(:@target), element, trail + ['target'])
        nested_element = array_elements(target)
        return Shape.array(Shape.never) if nested_element.nil?
        infer_projection_rhs(node.instance_variable_get(:@projection), nested_element, trail + ['[]'])
      when Nodes::ObjectProjection
        infer_object_projection(node, element, trail)
      when Nodes::Flatten
        flatten_shape(infer_element_chain(node.instance_variable_get(:@child), element, trail + ['target']))
      when Nodes::Slice
        infer_slice(node, element, trail)
      else
        infer(node, element, trail)
      end
    end

    # After one link of a post-projection chain, decide whether the next link
    # still sees an element or has reached a produced array.
    def continue_chain(right_node, middle, trail)
      case right_node
      when Nodes::ArrayProjection
        # The left produced something fed into another projection (rare here).
        infer_element_chain(right_node, middle, trail)
      else
        infer(right_node, middle, trail)
      end
    end

    def infer_object_projection(node, shape, trail)
      target = infer(node.instance_variable_get(:@target), shape, trail + ['target'])
      element = object_elements(target)
      return Shape.null if element.nil?

      projection = node.instance_variable_get(:@projection)
      projected =
        if projection.is_a?(Nodes::Condition)
          narrowed = apply_filter(element, projection, trail + ['.*'])
          infer_projection_child(projection.instance_variable_get(:@child),
                                 narrowed, trail + ['.*'])
        else
          infer_projection_child(projection, element, trail + ['.*'])
        end

      result = projected.array? ? projected : Shape.array(projected.remove_null)
      definitely_object?(target) ? result : result.add_null
    end

    def definitely_object?(target)
      target.object? || (target.union? && target.members.all?(&:object?))
    end

    # Shape of values reachable via an object wildcard; nil when the target is
    # definitely not an object.
    def object_elements(target)
      if target.object?
        if target.never?
          Shape.never
        elsif target.open?
          Shape.any
        else
          Shape.union(target.fields.values.map(&:value_shape))
        end
      elsif target.any?
        Shape.any
      elsif target.null_kinding?
        Shape.never
      elsif target.union?
        return nil if target.members.any? { |member| !member.object? && !member.any? && !member.null_kinding? }
        values = target.members.map { |member| object_elements(member) }.reject { |value| value.nil? || value.never? }
        values.empty? ? Shape.never : Shape.union(values)
      end
    end

    def flatten_shape(target)
      if target.never?
        Shape.never
      elsif target.array?
        element = target.element
        element.array? ? element.element : element
      elsif target.any?
        Shape.nullable(Shape.array(Shape.any))
      elsif target.null_kinding?
        Shape.null
      elsif target.union?
        Shape.union(target.members.map { |member| flatten_shape(member) })
      else
        Shape.null
      end
    end

    def infer_slice(node, shape, trail)
      if shape.array?
        Shape.array(shape.element)
      elsif shape.any?
        Shape.nullable(Shape.array(Shape.any))
      elsif shape.union?
        Shape.union(shape.members.map { |member| infer_slice(node, member, trail) })
      elsif shape.null_kinding?
        Shape.null
      else
        note(node, trail,
             'slice applied to a non-array value',
             ["value is `#{shape}`", 'slice yields null at runtime'])
        Shape.null
      end
    end

    def infer_condition(node, shape, trail)
      test = node.instance_variable_get(:@test)
      child = node.instance_variable_get(:@child)
      infer(test, shape, trail + ['filter'])
      narrowed = narrow(shape, test, trail + ['filter'])
      infer(child, narrowed, trail + ['branch'])
    end

    # Type-check the filter predicate and narrow the element shape for the
    # surviving branch.
    def apply_filter(element, condition, trail)
      test = condition.instance_variable_get(:@test)
      infer(test, element, trail + ['filter'])
      narrow(element, test, trail + ['filter'])
    end

    # ==================================================================
    # Filter narrowing
    # ==================================================================

    # Remove values the filter predicate proves cannot survive.
    def narrow(shape, test, trail)
      case test
      when Nodes::Comparators::Eq
        apply_comparator(shape, test, :eq, trail)
      when Nodes::Comparators::Neq
        apply_comparator(shape, test, :neq, trail)
      when Nodes::Comparators::Gt, Nodes::Comparators::Gte,
           Nodes::Comparators::Lt, Nodes::Comparators::Lte
        apply_comparator(shape, test, comparator_relation(test), trail)
      when Nodes::Not
        negate_comparator(shape, test.instance_variable_get(:@expression), trail)
      when Nodes::And
        narrow(narrow(shape, test.instance_variable_get(:@left), trail),
               test.instance_variable_get(:@right), trail)
      when Nodes::Or
        Shape.union(narrow(shape, test.instance_variable_get(:@left), trail + ['or-left']),
                    narrow(shape, test.instance_variable_get(:@right), trail + ['or-right']))
      else
        truthy_narrow(shape, test)
      end
    end

    # !(a == b) narrows like a != b, !(a != b) like a == b.
    def negate_comparator(shape, inner, _trail)
      case inner
      when Nodes::Comparators::Eq then apply_comparator(shape, inner, :neq, [])
      when Nodes::Comparators::Neq then apply_comparator(shape, inner, :eq, [])
      else shape
      end
    end

    # Handle one comparison. Either side may be a literal or a path rooted at
    # the current element; constrain the path side against the other side.
    def apply_comparator(shape, comparator, relation, trail)
      left_path = path_of(comparator.left)
      right_path = path_of(comparator.right)
      return shape unless left_path || right_path

      target_path = left_path || right_path
      other_node = left_path ? comparator.right : comparator.left
      target = comparison_target(other_node, shape)
      return shape if target.nil?

      apply_path(shape, target_path) { |field_shape| constrain(field_shape, target, relation) }
    end

    # The shape a path is compared against: a literal shape, the shape at
    # another path, or nil when neither can be determined.
    def comparison_target(node, root_shape)
      literal = literal_of(node)
      return literal if literal

      path = path_of(node)
      return nil unless path
      resolve_path(root_shape, path)
    end

    def resolve_path(shape, path)
      return shape if path.empty?
      head, *rest = path
      result = shape.lookup(head)
      return nil if result.nil?
      rest.empty? ? result : resolve_path(result, rest)
    end

    # Bare truthiness (field/projection) only proves null values are removed;
    # empty containers and empty strings are ignored here to stay sound.
    def truthy_narrow(shape, test)
      path = path_of(test)
      path ? apply_path(shape, path) { |field_shape| field_shape.remove_null } : shape
    end

    def comparator_relation(test)
      {
        Nodes::Comparators::Gt => :gt,
        Nodes::Comparators::Gte => :gte,
        Nodes::Comparators::Lt => :lt,
        Nodes::Comparators::Lte => :lte
      }.fetch(test.class)
    end

    # @return [Array] chain of field/index keys from current node, or nil
    #   when the expression is not a simple current-rooted path.
    def path_of(node)
      case node
      when Nodes::Current
        []
      when Nodes::Field, Nodes::Index
        [node.instance_variable_get(:@key)]
      when Nodes::Subexpression
        left_path = path_of(node.instance_variable_get(:@left))
        right_path = path_of(node.instance_variable_get(:@right))
        left_path && right_path ? left_path + right_path : nil
      end
    end

    # @return [Shape, nil] shape of a literal RHS, or nil for non-literals.
    def literal_of(node)
      node.is_a?(Nodes::Literal) ? Shape.from_value(node.value) : nil
    end

    def constrain(shape, literal, relation)
      return shape if literal.nil?

      case relation
      when :eq
        shape.intersect(literal)
      when :neq
        literal.null_kinding? ? shape.remove_null : shape
      when :gt, :gte, :lt, :lte
        if literal.number?
          keep_kinds(shape, Shape::NUMBER)
        elsif literal.string?
          keep_kinds(shape, Shape::STRING)
        else
          shape
        end
      else
        shape
      end
    end

    def keep_kinds(shape, kind)
      kept = shape.members.select { |member| member.kind == kind || member.any? }
      kept.empty? ? Shape.never : Shape.union(kept)
    end

    # Rewrite the shape reached via `path` using the block, leaving the rest
    # of the structure intact. Never turns +any+ into +never+.
    def apply_path(shape, path, &block)
      return shape if shape.any?
      return block.call(shape) if path.empty?

      head, *rest = path
      case shape
      when Shape
        case shape.kind
        when Shape::NEVER_KIND
          shape
        when :union
          Shape.union(shape.members.map { |member| apply_path(member, path, &block) })
        when Shape::OBJECT
          apply_object_path(shape, head, rest, &block)
        when Shape::ARRAY
          apply_array_path(shape, head, rest, &block)
        when Shape::NULL
          shape
        else
          # Scalar with a non-empty path: path does not exist, constraint
          # removes nothing observable.
          shape
        end
      end
    end

    def apply_object_path(shape, head, rest, &block)
      field = shape.fields[head.to_s]
      fields = shape.fields.dup
      if field
        new_field_shape = rest.empty? ? block.call(field.shape) : apply_path(field.shape, rest, &block)
        # The element only survives when this constraint holds, so a field
        # that is present and non-null here is required on the survivors.
        required = new_field_shape.nullable? ? field.required? : true
        fields[head.to_s] = Shape::Field.new(new_field_shape, required: required)
      elsif shape.open?
        # Unknown open field: assume null | any; constraining can only
        # remove null, never prove absence.
        candidate = rest.empty? ? block.call(Shape.nullable(Shape.any)) : Shape.nullable(Shape.any)
        fields[head.to_s] = Shape::Field.new(candidate, required: false)
      else
        return shape
      end
      Shape.object(fields, open: shape.open?)
    end

    def apply_array_path(shape, head, rest, &block)
      if head.is_a?(Integer)
        new_element = rest.empty? ? block.call(shape.element) : apply_path(shape.element, rest, &block)
        Shape.array(new_element)
      else
        shape
      end
    end

    # ==================================================================
    # Function calls
    # ==================================================================

    # Function classes register themselves into the shared FUNCTIONS hash on
    # Nodes::Function (name => class); invert it to recover the JMESPath name.
    FUNCTION_NAMES = Nodes::Function::FUNCTIONS.invert.freeze

    ARITY = {
      'abs' => 1..1, 'avg' => 1..1, 'ceil' => 1..1, 'contains' => 2..2,
      'floor' => 1..1, 'length' => 1..1, 'map' => 2..2, 'max' => 1..1,
      'min' => 1..1, 'type' => 1..1, 'keys' => 1..1, 'values' => 1..1,
      'join' => 2..2, 'to_string' => 1..1, 'to_number' => 1..1,
      'sum' => 1..1, 'not_null' => 1..nil, 'sort' => 1..1,
      'sort_by' => 2..2, 'max_by' => 2..2, 'min_by' => 2..2,
      'ends_with' => 2..2, 'starts_with' => 2..2, 'merge' => 1..nil,
      'reverse' => 1..1, 'to_array' => 1..1
    }.freeze

    def infer_function(node, shape, trail)
      name = function_name(node)
      children = node.instance_variable_get(:@children)

      unless arity_ok?(name, children)
        children.each { |child| infer(child, shape, trail + ['arg']) }
        error(node, trail,
              "function #{name}() called with #{children.size} argument(s)",
              ["#{name}() accepts #{arity_description(name)} argument(s)"])
        return Shape.any
      end

      send("fn_#{name}", node, children, shape, trail)
    end

    def arity_ok?(name, children)
      ARITY.fetch(name, 1..nil) === children.size
    end

    def arity_description(name)
      range = ARITY.fetch(name, 1..nil)
      range.end.nil? ? "#{range.begin} or more" : range.to_s
    end

    def function_name(node)
      FUNCTION_NAMES.fetch(node.class)
    end

    def arg_shape(child, root_shape, trail)
      infer(child, root_shape, trail + ['arg'])
    end

    # Split union members into accepted / rejected / unknown buckets.
    def partition_kinds(shape, acceptable)
      accepted = []
      rejected = []
      unknown = 0
      shape.members.each do |member|
        if member.any?
          unknown += 1
        elsif acceptable.include?(member.kind)
          accepted << member
        else
          rejected << member
        end
      end
      [accepted, rejected, unknown]
    end

    # Check a single-kind argument. An error is definite only when every
    # concrete possibility is rejected with no unknown slack.
    def check_kind(node, trail, name, argument, acceptable, message, reasons)
      accepted, rejected, unknown = partition_kinds(argument, acceptable)

      if rejected.any? && accepted.empty? && unknown.zero?
        error(node, trail, message,
              reasons + ["argument is `#{argument}`",
                         "acceptable types: #{kind_names(acceptable)}"])
      elsif rejected.any? || unknown.positive?
        parts = rejected.map(&:to_s)
        parts << 'any (unknown)' if unknown.positive?
        note(node, trail,
             "#{name}() argument is not statically guaranteed to be #{kind_names(acceptable)}",
             ["argument can be: #{parts.join(', ')}",
              'runtime type validation is kept'])
      end
      accepted.empty? && unknown.positive? ? Shape.any : Shape.union(accepted)
    end

    # Decompose a possibly-union argument into array elements.
    # @return [element_shape, non_array_members, any_slack, has_null]
    def array_argument(shape)
      elements = []
      non_array = []
      any_slack = 0
      has_null = false

      shape.members.each do |member|
        case member.kind
        when Shape::ARRAY
          member.element.members.each { |element| elements << element }
        when Shape::ANY_KIND
          any_slack += 1
        when Shape::NULL
          has_null = true
        else
          non_array << member
        end
      end

      [Shape.union(elements), non_array, any_slack, has_null]
    end

    # Inspect array elements against the acceptable kinds.
    # Returns :ok (all acceptable), :runtime (mixed/unknown) or
    # :error (every concrete element is definitely unacceptable).
    def check_uniform_elements(node, trail, name, element_shape, acceptable)
      accepted, rejected, unknown = partition_kinds(element_shape, acceptable)
      return :ok if rejected.empty? && unknown.zero?

      if rejected.any? && accepted.empty? && unknown.zero? && !element_shape.null_kinding?
        error(node, trail,
              "function #{name}() expects elements to be #{kind_names(acceptable)}",
              ["JMESPath #{name}() raises invalid-type for other element types",
               "elements are `#{element_shape}`",
               "acceptable element types: #{kind_names(acceptable)}"])
        :error
      else
        note(node, trail,
             "#{name}() elements are not statically guaranteed to be #{kind_names(acceptable)}",
             ["elements can be: #{element_shape}",
              'per-element type mismatches are reported at runtime'])
        :runtime
      end
    end

    def kind_names(kinds)
      kinds.map(&:to_s).join(' or ')
    end

    # ----- abs / ceil / floor ------------------------------------------

    def fn_abs(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      check_kind(node, trail, 'abs', argument, [Shape::NUMBER],
                 'function abs() expects a number',
                 ['JMESPath abs() raises invalid-type for non-numbers'])
      Shape.number
    end

    def fn_ceil(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      check_kind(node, trail, 'ceil', argument, [Shape::NUMBER],
                 'function ceil() expects a number',
                 ['JMESPath ceil() raises invalid-type for non-numbers'])
      Shape.number
    end

    def fn_floor(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      check_kind(node, trail, 'floor', argument, [Shape::NUMBER],
                 'function floor() expects a number',
                 ['JMESPath floor() raises invalid-type for non-numbers'])
      Shape.number
    end

    # ----- avg / sum ----------------------------------------------------

    def fn_avg(node, children, shape, trail)
      number_array(node, children, shape, trail, 'avg')
      Shape.nullable(Shape.number)
    end

    def fn_sum(node, children, shape, trail)
      number_array(node, children, shape, trail, 'sum')
      Shape.number
    end

    def number_array(node, children, shape, trail, name)
      argument = arg_shape(children.first, shape, trail)
      element, non_array, any_slack, = array_argument(argument)

      if non_array.any? && any_slack.zero?
        error(node, trail,
              "function #{name}() expects an array",
              ["JMESPath #{name}() raises invalid-type for non-arrays",
               "argument can be: #{non_array.map(&:to_s).join(', ')}"])
      elsif non_array.any? || any_slack.positive?
        parts = non_array.map(&:to_s)
        parts << 'any (unknown)' if any_slack.positive?
        note(node, trail,
             "function #{name}() argument is not statically guaranteed to be an array",
             ["argument can be: #{parts.join(', ')}",
              'runtime type validation is kept'])
      end
      check_uniform_elements(node, trail, name, element, [Shape::NUMBER])
    end

    # ----- min / max ----------------------------------------------------

    def fn_max(node, children, shape, trail)
      ordered_array(node, children, shape, trail, 'max')
    end

    def fn_min(node, children, shape, trail)
      ordered_array(node, children, shape, trail, 'min')
    end

    def ordered_array(node, children, shape, trail, name)
      argument = arg_shape(children.first, shape, trail)
      element, non_array, any_slack, = array_argument(argument)

      if non_array.any? && any_slack.zero?
        error(node, trail,
              "function #{name}() expects an array",
              ["argument can be: #{non_array.map(&:to_s).join(', ')}"])
      end
      check_uniform_elements(node, trail, name, element, [Shape::NUMBER, Shape::STRING])
      Shape.nullable(Shape.union(Shape.number, Shape.string))
    end

    # ----- sort ---------------------------------------------------------

    def fn_sort(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      element, non_array, any_slack, = array_argument(argument)

      if non_array.any? && any_slack.zero?
        error(node, trail,
               'function sort() expects an array',
               ["argument can be: #{non_array.map(&:to_s).join(', ')}"])
      end
      check_uniform_elements(node, trail, 'sort', element, [Shape::NUMBER, Shape::STRING])
      Shape.nullable(Shape.array(element))
    end

    # ----- sort_by / max_by / min_by ------------------------------------

    def fn_sort_by(node, children, shape, trail)
      by_function(node, children, shape, trail, 'sort_by', as_array: true)
    end

    def fn_max_by(node, children, shape, trail)
      by_function(node, children, shape, trail, 'max_by', as_array: false)
    end

    def fn_min_by(node, children, shape, trail)
      by_function(node, children, shape, trail, 'min_by', as_array: false)
    end

    def by_function(node, children, shape, trail, name, as_array:)
      list_shape = arg_shape(children.first, shape, trail)
      expression_node = children[1]

      unless expression_node.is_a?(Nodes::Expression)
        actual = arg_shape(expression_node, shape, trail)
        error(node, trail,
              "function #{name}() expects an expression as its second argument",
              ['JMESPath requires an expression reference (&...)',
               "second argument is `#{actual}`"])
        return Shape.any
      end

      element, non_array, any_slack, = array_argument(list_shape)
      if non_array.any? && any_slack.zero?
        error(node, trail,
              "function #{name}() expects an array as its first argument",
              ["argument can be: #{non_array.map(&:to_s).join(', ')}"])
        return Shape.any
      end

      element = Shape.any if element.never?
      keyed = infer(expression_node.instance_variable_get(:@expression),
                    element, trail + ['expression'])
      check_uniform_elements(node, trail, name, keyed, [Shape::NUMBER, Shape::STRING])

      result = as_array ? Shape.array(element) : element
      (non_array.any? || any_slack.positive?) ? result.add_null : result
    end

    # ----- map ----------------------------------------------------------

    def fn_map(node, children, shape, trail)
      expression_node = children.first
      list_shape = arg_shape(children[1], shape, trail)

      unless expression_node.is_a?(Nodes::Expression)
        actual = arg_shape(expression_node, shape, trail)
        error(node, trail,
               'function map() expects an expression as its first argument',
               ['JMESPath requires an expression reference (&...)',
                "first argument is `#{actual}`"])
        return Shape.any
      end

      element, non_array, any_slack, = array_argument(list_shape)
      if non_array.any? && any_slack.zero?
        error(node, trail,
               'function map() expects a list as its second argument',
               ["argument can be: #{non_array.map(&:to_s).join(', ')}"])
        return Shape.any
      end

      element = Shape.any if element.never?
      mapped = map_expression(expression_node, element, trail + ['expression'])
      result = Shape.array(mapped)
      (non_array.any? || any_slack.positive?) ? result.add_null : result
    end

    # Apply a map expression to a single element. A projection expression
    # already yields an array; a plain value expression yields one value.
    def map_expression(expression_node, element, trail)
      expr = expression_node.instance_variable_get(:@expression)
      result = infer(expr, element, trail)
      result.array? ? result.element : result
    end

    # ----- contains / length / keys / values ----------------------------

    def fn_contains(node, children, shape, trail)
      haystack = arg_shape(children.first, shape, trail)
      arg_shape(children[1], shape, trail)
      check_kind(node, trail, 'contains', haystack, [Shape::ARRAY, Shape::STRING],
                 'function contains() expects a string or array as first argument',
                 ['JMESPath contains() raises invalid-type otherwise'])
      Shape.boolean
    end

    def fn_length(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      check_kind(node, trail, 'length', argument,
                 [Shape::STRING, Shape::ARRAY, Shape::OBJECT],
                 'function length() expects a string, array or object',
                 ['JMESPath length() raises invalid-type otherwise'])
      Shape.number
    end

    def fn_keys(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      check_kind(node, trail, 'keys', argument, [Shape::OBJECT],
                 'function keys() expects an object',
                 ['JMESPath keys() raises invalid-type for non-objects'])
      Shape.nullable(Shape.array(Shape.string))
    end

    def fn_values(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      accepted, rejected, unknown = partition_kinds(argument, [Shape::OBJECT, Shape::ARRAY])

      if rejected.any? && accepted.empty? && unknown.zero?
        error(node, trail,
              'function values() expects an array or an object',
              ["argument is `#{argument}`"])
        return Shape.any
      elsif rejected.any? || unknown.positive?
        note(node, trail,
             'values() argument is not statically guaranteed to be an array or object',
             ["argument can be: #{argument}", 'runtime type validation is kept'])
      end

      parts = argument.members.map do |member|
        if member.object?
          member.open? ? Shape.any : Shape.union(member.fields.values.map(&:value_shape))
        elsif member.array?
          member.element
        else
          Shape.any
        end
      end
      Shape.nullable(Shape.array(Shape.union(parts)))
    end

    # ----- join / reverse -----------------------------------------------

    def fn_join(node, children, shape, trail)
      glue = arg_shape(children.first, shape, trail)
      values = arg_shape(children[1], shape, trail)

      check_kind(node, trail, 'join', glue, [Shape::STRING],
                 'function join() expects the first argument to be a string',
                 ['JMESPath join() raises invalid-type otherwise'])

      element, non_array, any_slack, = array_argument(values)
      if non_array.any? && any_slack.zero?
        error(node, trail,
              'function join() expects an array of strings as second argument',
              ["second argument can be: #{non_array.map(&:to_s).join(', ')}"])
        return Shape.any
      end
      check_uniform_elements(node, trail, 'join', element, [Shape::STRING])
      Shape.string
    end

    def fn_reverse(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      accepted, rejected, unknown = partition_kinds(argument, [Shape::ARRAY, Shape::STRING])

      if rejected.any? && accepted.empty? && unknown.zero?
        error(node, trail,
              'function reverse() expects an array or string',
              ["argument is `#{argument}`"])
        return Shape.any
      elsif rejected.any? || unknown.positive?
        note(node, trail,
             'reverse() argument is not statically guaranteed to be an array or string',
             ["argument can be: #{argument}", 'runtime type validation is kept'])
      end

      parts = argument.members.map do |member|
        if member.array?
          Shape.array(member.element)
        elsif member.string?
          Shape.string
        else
          Shape.any
        end
      end
      Shape.union(parts)
    end

    # ----- merge --------------------------------------------------------

    def fn_merge(node, children, shape, trail)
      children.map { |child| arg_shape(child, shape, trail) }.each_with_index
             .then { |pairs| merge_fields(node, trail, pairs) }
    end

    def merge_fields(node, trail, pairs)
      fields = {}
      open = false
      rejected = []

      pairs.each do |argument, index|
        argument.members.each do |member|
          if member.object?
            open ||= member.open?
            member.fields.each do |name, field|
              current = fields[name]
              fields[name] = if current
                               Shape::Field.new(
                                 Shape.union(current.shape, field.shape),
                                 required: current.required? && field.required?
                               )
                             else
                               field
                             end
            end
          elsif member.any?
            open = true
          elsif member.null_kinding?
            rejected << member
          else
            rejected << member
          end
        end
      end

      if rejected.any?
        note(node, trail,
             'merge() argument is not statically guaranteed to be an object',
             ["arguments can be: #{rejected.map(&:to_s).uniq.join(', ')}",
              'JMESPath merge() raises invalid-type for non-objects at runtime'])
      end
      Shape.object(fields, open: open)
    end

    # ----- type / to_string / to_number / to_array ----------------------

    def fn_type(node, children, shape, trail)
      arg_shape(children.first, shape, trail)
      Shape.string
    end

    def fn_to_string(node, children, shape, trail)
      arg_shape(children.first, shape, trail)
      Shape.string
    end

    def fn_to_number(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      if argument.definitely?(Shape::NUMBER)
        Shape.number
      else
        note(node, trail,
             'to_number() may return null for non-numeric values',
             ["argument can be: #{argument}",
              'JMESPath to_number() returns null when conversion fails'])
        Shape.nullable(Shape.number)
      end
    end

    def fn_to_array(node, children, shape, trail)
      argument = arg_shape(children.first, shape, trail)
      parts = argument.members.map do |member|
        if member.array?
          member
        elsif member.never?
          Shape.array(Shape.never)
        else
          Shape.array(member)
        end
      end
      Shape.union(parts)
    end

    # ----- not_null -----------------------------------------------------

    def fn_not_null(node, children, shape, trail)
      shapes = children.map { |child| arg_shape(child, shape, trail) }
      without_null = shapes.map(&:remove_null)
      Shape.union(without_null)
    end

    # ----- ends_with / starts_with --------------------------------------

    def fn_ends_with(node, children, shape, trail)
      two_strings(node, children, shape, trail, 'ends_with')
    end

    def fn_starts_with(node, children, shape, trail)
      two_strings(node, children, shape, trail, 'starts_with')
    end

    def two_strings(node, children, shape, trail, name)
      first = arg_shape(children.first, shape, trail)
      second = arg_shape(children[1], shape, trail)
      check_kind(node, trail, name, first, [Shape::STRING],
                 "function #{name}() expects the first argument to be a string",
                 ["JMESPath #{name}() raises invalid-type otherwise"])
      check_kind(node, trail, name, second, [Shape::STRING],
                 "function #{name}() expects the second argument to be a string",
                 ["JMESPath #{name}() raises invalid-type otherwise"])
      Shape.boolean
    end

    # ==================================================================
    # Findings
    # ==================================================================

    def error(node, trail, message, reasons)
      @findings << Finding.new(
        severity: :error,
        node_class: node.class,
        trail: trail.dup,
        message: message,
        reasons: reasons
      )
    end

    def note(node, trail, message, reasons)
      @findings << Finding.new(
        severity: :runtime,
        node_class: node.class,
        trail: trail.dup,
        message: message,
        reasons: reasons
      )
    end
  end
end
