# frozen_string_literal: true

require 'set'

module Liquid
  # A DropAccessPolicy narrows the set of properties a template may read from
  # drops during a single render. It is meant for cases where the same drop is
  # exposed to templates with different trust levels (for example internal
  # templates vs. user uploaded templates) and each audience may only see a
  # subset of the available fields.
  #
  # Rules are registered per drop class and may be nested to also constrain the
  # values a field returns (arrays, hashes or other drops):
  #
  #   policy = Liquid::DropAccessPolicy.new
  #   policy.allow(ProductDrop, 'title', 'vendor' => ['name'])
  #
  #   template.render('product' => product_drop, drop_access_policy: policy)
  #
  # With the policy above, `product.title` and `product.vendor` are readable,
  # and the value returned by `product.vendor` (whatever its type) only exposes
  # `name`. Rules registered for a drop class apply to that drop wherever it
  # appears in a lookup path; nested rules additionally restrict the value
  # found at that path. When both apply, the value must satisfy the
  # intersection of the two.
  #
  # Denied properties are never invoked: the policy is consulted before the
  # drop method (or hash/array entry) is read, so a rejected access has no side
  # effects. In strict variable mode a denied access raises
  # Liquid::UndefinedDropMethod (for drops) or Liquid::UndefinedVariable
  # (for hashes); otherwise it evaluates to nil, exactly like an unknown
  # property would.
  #
  # The policy is enforced at render time through the context. It does not
  # add, remove or redefine any method on the drop classes themselves, so
  # renders without a policy (or with a different one) are unaffected.
  class DropAccessPolicy
    # A Rule describes which keys may be read from an object and, optionally,
    # which rules apply to the values returned for specific keys.
    # `fields` is a Set of allowed key names, or nil when every key is allowed.
    class Rule
      attr_reader :fields, :children

      def initialize(fields = nil, children = {})
        @fields   = fields
        @children = children
      end

      def allows?(key)
        @fields.nil? || @fields.include?(key)
      end

      def child(key)
        @children[key]
      end

      # Narrows two rules down to the keys both of them allow.
      def self.intersect(left, right)
        return left if right.nil?
        return right if left.nil?

        fields = if left.fields.nil?
          right.fields
        elsif right.fields.nil?
          left.fields
        else
          left.fields & right.fields
        end

        children = {}
        left.children.each_key do |key|
          children[key] = intersect(left.children[key], right.children[key])
        end
        right.children.each do |key, child|
          children[key] ||= intersect(nil, child)
        end

        new(fields, children)
      end

      # Combines two rules so that everything either of them allows stays
      # allowed. Used when a class is registered more than once.
      def union(other)
        fields = if @fields.nil? || other.fields.nil?
          nil
        else
          @fields | other.fields
        end

        children = @children.dup
        other.children.each do |key, child|
          children[key] = children.key?(key) ? children[key].union(child) : child
        end

        Rule.new(fields, children)
      end
    end

    def initialize
      @class_rules = {}
      @resolved    = {}
    end

    # Allows +fields+ on +drop_class+. Nested rules for the values returned by
    # a field are passed as a trailing hash:
    #
    #   policy.allow(ProductDrop, 'title', 'vendor' => ['name'])
    #
    # Calling allow again for the same class adds to the previously allowed
    # fields.
    def allow(drop_class, *fields, **nested)
      rule = build_rule(fields, nested)
      existing = @class_rules[drop_class]
      @class_rules[drop_class] = existing ? existing.union(rule) : rule
      @resolved.clear
      self
    end

    # The rule that constrains reading properties from +object+, combining the
    # rules registered for its class (when it is a drop) with the +inherited+
    # rule imposed by the path through which the object was reached.
    # Returns nil when the object is unconstrained.
    def rule_for(object, inherited = nil)
      class_rule = object.is_a?(Liquid::Drop) ? class_rule_for(object.class) : nil
      Rule.intersect(class_rule, inherited)
    end

    # Whether +key+ may be read from +object+ under +rule+. Array elements
    # stay constrained by the same rule, so index and command access to the
    # array itself is always permitted.
    def key_allowed?(rule, object, key)
      return true if rule.nil? || object.is_a?(Array)

      rule.allows?(key.to_s)
    end

    # The rule constraining the value fetched with +key+ from +object+.
    def child_rule(rule, object, key)
      return nil if rule.nil?
      return rule if object.is_a?(Array)

      rule.child(key.to_s)
    end

    private

    def class_rule_for(klass)
      return @resolved[klass] if @resolved.key?(klass)

      ancestor = klass.ancestors.find { |mod| mod.is_a?(Class) && @class_rules.key?(mod) }
      @resolved[klass] = ancestor ? @class_rules[ancestor] : nil
    end

    def build_rule(fields, nested)
      children = {}
      nested.each do |key, value|
        children[key.to_s] = build_nested_rule(value)
      end
      Rule.new((fields.map(&:to_s) | children.keys).to_set, children)
    end

    def build_nested_rule(value)
      fields   = []
      children = {}

      Array(value).each do |entry|
        if entry.is_a?(Hash)
          entry.each do |key, nested|
            children[key.to_s] = build_nested_rule(nested)
            fields << key.to_s
          end
        else
          fields << entry.to_s
        end
      end

      Rule.new(fields.to_set, children)
    end
  end
end
