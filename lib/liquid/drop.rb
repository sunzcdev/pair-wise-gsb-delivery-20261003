# frozen_string_literal: true

require 'set'

module Liquid
  # A drop in liquid is a class which allows you to export DOM like things to liquid.
  # Methods of drops are callable.
  # The main use for liquid drops is to implement lazy loaded objects.
  # If you would like to make data available to the web designers which you don't want loaded unless needed then
  # a drop is a great way to do that.
  #
  # Example:
  #
  #   class ProductDrop < Liquid::Drop
  #     def top_sales
  #       Shop.current.products.find(:all, :order => 'sales', :limit => 10 )
  #     end
  #   end
  #
  #   tmpl = Liquid::Template.parse( ' {% for product in product.top_sales %} {{ product.name }} {%endfor%} '  )
  #   tmpl.render('product' => ProductDrop.new ) # will invoke top_sales query.
  #
  # Your drop can either implement the methods sans any parameters
  # or implement the liquid_method_missing(name) method which is a catch all.
  class Drop
    attr_writer :context

    def initialize
      @context = nil
    end

    # Catch all for the method
    def liquid_method_missing(method)
      return unless @context&.strict_variables
      raise Liquid::UndefinedDropMethod, "undefined method #{method}"
    end

    # called by liquid to invoke a drop
    def invoke_drop(method_or_key)
      policy = @context&.drop_access_policy
      node   = policy&.node_for(self)

      if node && !node.key?(method_or_key.to_s)
        # Denied by the render's access policy. Reject before any method is
        # invoked, mirroring the strict/non-strict variable behavior.
        return nil unless @context&.strict_variables
        raise Liquid::UndefinedDropMethod, "undefined method #{method_or_key}"
      end

      result = if self.class.invokable?(method_or_key)
        send(method_or_key)
      else
        liquid_method_missing(method_or_key)
      end

      if node && (sub_node = node[method_or_key.to_s])
        result = policy.constrain_value(result, sub_node)
      end

      result
    end

    def key?(_name)
      true
    end

    def inspect
      self.class.to_s
    end

    def to_liquid
      self
    end

    def to_s
      self.class.name
    end

    alias_method :[], :invoke_drop

    # @private Called by Liquid::DropAccessPolicy to scope this drop to a
    # rule node for the duration of a render.
    def liquid_constrain_access(policy, node)
      @liquid_access_scope = [policy, node]
    end

    # @private Returns the rule node this drop was scoped to by the given
    # policy, or nil when it is not scoped under that policy.
    def liquid_access_scope_for(policy)
      scope = @liquid_access_scope
      scope[1] if scope && scope[0].equal?(policy)
    end

    # Check for method existence without invoking respond_to?, which creates symbols
    def self.invokable?(method_name)
      invokable_methods.include?(method_name.to_s)
    end

    def self.invokable_methods
      @invokable_methods ||= begin
        blacklist = Liquid::Drop.public_instance_methods + [:each]

        if include?(Enumerable)
          blacklist += Enumerable.public_instance_methods
          blacklist -= [:sort, :count, :first, :min, :max]
        end

        whitelist = [:to_liquid] + (public_instance_methods - blacklist)
        Set.new(whitelist.map(&:to_s))
      end
    end
  end
end
