# frozen_string_literal: true

module Liquid
  # A DropAccessPolicy narrows the visible attributes of drops for a single
  # render. It is useful when the same drop backs templates with different
  # trust levels (e.g. internal templates vs. uploaded ones) and each audience
  # may only see a subset of the fields.
  #
  # Rules are registered per drop class as attribute paths. Nested paths
  # constrain the values returned by an attribute, whether they are drops,
  # arrays of drops, or hashes:
  #
  #   policy = Liquid::DropAccessPolicy.new
  #   policy.allow(ProductDrop, "title", "reviews.rating")
  #
  #   template.render({ "product" => ProductDrop.new }, drop_access_policy: policy)
  #
  # With this policy, `product.title` and `product.reviews` are visible, but
  # every review reached through `product.reviews` only exposes `rating`.
  # Drop classes without any registered rule are left unconstrained.
  #
  # Denied attributes are rejected before the underlying method is invoked,
  # so methods with side effects are never called for a denied path. In
  # strict variable mode a denied attribute raises
  # `Liquid::UndefinedDropMethod`; otherwise it evaluates to `nil`.
  #
  # The policy never modifies the drop classes themselves; it is only
  # consulted while rendering with a context that carries the policy.
  class DropAccessPolicy
    def initialize(rules = {})
      @rules = {}
      @resolved = {}
      rules.each do |drop_class, paths|
        allow(drop_class, *Array(paths))
      end
    end

    # Allows the given attribute paths for a drop class. A path is a dot
    # separated list of attribute names, e.g. "reviews.rating". Allowing a
    # nested path implicitly allows each intermediate attribute, while the
    # value returned by the last intermediate attribute stays constrained to
    # the remainder of the path.
    def allow(drop_class, *paths)
      node = (@rules[drop_class] ||= {})
      @resolved.clear

      paths.flatten.each do |path|
        parts = path.to_s.split(".")
        parts.each_with_index.reduce(node) do |current, (part, index)|
          if index == parts.length - 1
            current[part] = nil unless current.key?(part)
          else
            current[part] = {} unless current[part].is_a?(Hash)
          end
          current[part]
        end
      end

      self
    end

    # Returns the rule node effective for the given drop instance, or nil when
    # the drop is unconstrained. A node is a hash mapping attribute names to
    # either nil (unconstrained value) or a nested node constraining the
    # returned value.
    def node_for(drop)
      scoped = drop.liquid_access_scope_for(self)
      return scoped if scoped

      type_node_for(drop.class)
    end

    # Applies a rule node to a value returned by an allowed attribute.
    # Drops are scoped so further lookups on them (within the same render)
    # stay constrained, arrays are constrained element-wise, and hashes are
    # re-built containing only the allowed keys.
    def constrain_value(value, node)
      case value
      when Liquid::Drop
        value.liquid_constrain_access(self, node)
        value
      when Array
        value.map { |element| constrain_value(element, node) }
      when Hash
        value.each_with_object({}) do |(key, element), constrained|
          name = key.to_s
          next unless node.key?(name)

          sub_node = node[name]
          constrained[key] = sub_node ? constrain_value(element, sub_node) : element
        end
      else
        value
      end
    end

    private

    def type_node_for(klass)
      return @resolved[klass] if @resolved.key?(klass)

      ancestor = klass
      while ancestor
        if @rules.key?(ancestor)
          return @resolved[klass] = @rules[ancestor]
        end
        ancestor = ancestor.superclass
      end

      @resolved[klass] = nil
    end
  end
end
