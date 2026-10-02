# frozen_string_literal: true

# String#safe_constantize, String#demodulize, String#underscore, String#camelize
require "active_support/core_ext/string/inflections"
# String#blank?, used by the inflections above
require "active_support/core_ext/object/blank"

module Pundit
  # Finds policy and scope classes for given object.
  # @since v0.1.0
  # @api public
  # @example
  #   user = User.find(params[:id])
  #   finder = PolicyFinder.new(user)
  #   finder.policy #=> UserPolicy
  #   finder.scope #=> UserPolicy::Scope
  #
  class PolicyFinder
    # A constant applied to the end of the class name to find the policy class.
    #
    # @api private
    # @since v2.5.0
    SUFFIX = "Policy"

    # @see #initialize
    # @since v0.1.0
    attr_reader :object

    # @see #initialize
    # @since v2.6.0
    attr_reader :namespaces

    # @param object [any] the object to find policy and scope classes for
    # @param namespaces [Array<Module, #to_s>] namespaces to search for the
    #   policy class, in order, before falling back to the top-level namespace.
    #   Only used when the object does not define its own `policy_class`.
    # @since v0.1.0
    def initialize(object, namespaces: [])
      @object = object
      @namespaces = namespaces
    end

    # @return [nil, Scope{#resolve}] scope class which can resolve to a scope
    # @see https://github.com/varvet/pundit#scopes
    # @example
    #   scope = finder.scope #=> UserPolicy::Scope
    #   scope.resolve #=> <#ActiveRecord::Relation ...>
    #
    # @since v0.1.0
    def scope
      "#{policy}::Scope".safe_constantize
    end

    # @return [nil, Class] policy class with query methods
    # @see https://github.com/varvet/pundit#policies
    # @example
    #   policy = finder.policy #=> UserPolicy
    #   policy.show? #=> true
    #   policy.update? #=> false
    #
    # @since v0.1.0
    def policy
      klass = find(object)
      klass.is_a?(String) ? find_in_namespaces(klass) : klass
    end

    # @return [Scope{#resolve}] scope class which can resolve to a scope
    # @raise [NotDefinedError] if scope could not be determined
    #
    # @since v0.1.0
    def scope!
      scope or raise NotDefinedError, "unable to find scope `#{find(object)}::Scope` for `#{object.inspect}`"
    end

    # @return [Class] policy class with query methods
    # @raise [NotDefinedError] if policy could not be determined
    #
    # @since v0.1.0
    def policy!
      policy or raise NotDefinedError, "unable to find policy `#{find(object)}` for `#{object.inspect}`"
    end

    # @return [String] the name of the key this object would have in a params hash
    #
    # @since v1.1.0
    def param_key # rubocop:disable Metrics/AbcSize
      model = object.is_a?(Array) ? object.last : object

      if model.respond_to?(:model_name)
        model.model_name.param_key.to_s
      elsif model.is_a?(Class)
        model.to_s.demodulize.underscore
      else
        model.class.to_s.demodulize.underscore
      end
    end

    private

    # Given an object, find the policy class name.
    #
    # Uses recursion to handle namespaces.
    #
    # @return [String, Class] the policy class, or its name.
    # @since v0.2.0
    def find(subject)
      if subject.is_a?(Array)
        modules = subject.dup
        last = modules.pop
        context = modules.map { |x| find_class_name(x) }.join("::")
        [context, find(last)].join("::")
      elsif subject.respond_to?(:policy_class)
        subject.policy_class
      elsif subject.class.respond_to?(:policy_class)
        subject.class.policy_class
      else
        klass = find_class_name(subject)
        "#{klass}#{SUFFIX}"
      end
    end

    # Given an object, find its' class name.
    #
    # - Supports ActiveModel.
    # - Supports regular classes.
    # - Supports symbols.
    # - Supports object instances.
    #
    # @return [String, Class] the class, or its name.
    # @since v1.1.0
    def find_class_name(subject)
      if subject.respond_to?(:model_name)
        subject.model_name
      elsif subject.class.respond_to?(:model_name)
        subject.class.model_name
      elsif subject.is_a?(Class)
        subject
      elsif subject.is_a?(Symbol)
        subject.to_s.camelize
      else
        subject.class
      end
    end

    # Given a policy class name, look it up in each namespace in order,
    # falling back to the top-level namespace.
    #
    # Only the absence of a candidate moves the lookup on to the next
    # namespace. Constants are resolved strictly within each namespace, so a
    # top-level policy is never picked up through another namespace.
    #
    # @api private
    # @param class_name [String] the policy class name to resolve
    # @return [nil, Class] the policy class, if found
    # @since v2.6.0
    def find_in_namespaces(class_name)
      namespaces.each do |namespace|
        candidate = strict_constantize(namespace, class_name)
        return candidate if candidate
      end

      class_name.safe_constantize
    end

    # Resolve a (possibly nested) class name strictly within a namespace,
    # without falling back to ancestors or the top-level namespace.
    #
    # @api private
    # @param namespace [Module, #to_s] the namespace to resolve within
    # @param class_name [String] the class name to resolve
    # @return [nil, Class] the resolved class, if defined in the namespace
    # @since v2.6.0
    def strict_constantize(namespace, class_name)
      namespace = namespace.to_s.safe_constantize unless namespace.is_a?(Module)
      return unless namespace

      class_name.split("::").reduce(namespace) do |context, name|
        return unless context.is_a?(Module) && name.match?(/\A[A-Z]\w*\z/) && context.const_defined?(name, false)

        context.const_get(name, false)
      end
    end
  end
end
