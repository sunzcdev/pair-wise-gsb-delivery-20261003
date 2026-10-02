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

    # @param object [any] the object to find policy and scope classes for
    # @param namespace [Module, Symbol, String, nil] namespace to prefix inferred policy names with
    # @since v0.1.0
    def initialize(object, namespace: nil)
      @object = object
      @namespace = namespace
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
      klass = find(object, @namespace)
      klass.is_a?(String) ? klass.safe_constantize : klass
    end

    # @return [Scope{#resolve}] scope class which can resolve to a scope
    # @raise [NotDefinedError] if scope could not be determined
    #
    # @since v0.1.0
    def scope!
      scope or raise NotDefinedError,
        "unable to find scope `#{find(object, @namespace)}::Scope` for `#{object.inspect}`"
    end

    # @return [Class] policy class with query methods
    # @raise [NotDefinedError] if policy could not be determined
    #
    # @since v0.1.0
    def policy!
      policy or raise NotDefinedError, "unable to find policy `#{find(object, @namespace)}` for `#{object.inspect}`"
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
    def find(subject, namespace = @namespace)
      if subject.is_a?(Array)
        modules = subject.dup
        last = modules.pop
        context = modules.map { |x| find_class_name(x) }.join("::")
        candidate = find(last, nil)
        return candidate if namespace.blank? && context.blank?
        return candidate if candidate.is_a?(Class) && candidate.name.blank?

        [namespace_name(namespace), context, candidate].reject(&:blank?).join("::")
      elsif subject.respond_to?(:policy_class)
        subject.policy_class
      elsif subject.class.respond_to?(:policy_class)
        subject.class.policy_class
      else
        klass = find_class_name(subject)
        [namespace_name(namespace), "#{klass}#{SUFFIX}"].reject(&:blank?).join("::")
      end
    end

    def namespace_name(namespace)
      return if namespace.nil?
      return namespace.name if namespace.is_a?(Module)

      find_class_name(namespace).to_s
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
      elsif subject.is_a?(Module)
        subject.name
      elsif subject.is_a?(Symbol)
        subject.to_s.camelize
      elsif subject.is_a?(String)
        subject.camelize
      else
        subject.class
      end
    end
  end
end
