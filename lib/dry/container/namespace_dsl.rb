# frozen_string_literal: true

require "delegate"

module Dry
  class Container
    # @api private
    class NamespaceDSL < ::SimpleDelegator
      # DSL for defining namespaces
      #
      # @param [Dry::Container::Mixin] container
      #   The container
      # @param [String] namespace
      #   The namespace (name)
      # @param [String] namespace_separator
      #   The namespace separator
      # @yield
      #   The block to evaluate to define the namespace
      #
      # @return [Mixed]
      #
      # @api private
      def initialize(container, namespace, namespace_separator, &block)
        @namespace = namespace
        @namespace_separator = namespace_separator

        super(container)

        if block.arity.zero?
          instance_eval(&block)
        else
          yield self
        end
      end

      def register(key, *args, &block)
        super(namespaced(key), *namespaced_options(args), &block)
      end

      def namespace(namespace, &block)
        super(namespaced(namespace), &block)
      end

      def import(namespace)
        namespace(namespace.name, &namespace.block)

        self
      end

      def resolve(key)
        super(namespaced(key))
      end

      private

      def namespaced(key)
        [@namespace, key].join(@namespace_separator)
      end

      def namespaced_options(args)
        args.map { |arg|
          arg.is_a?(::Hash) ? namespaced_dependency_options(arg) : arg
        }
      end

      def namespaced_dependency_options(options)
        options.each_with_object({}) do |(option, value), namespaced_options|
          namespaced_options[option] =
            if %i[depends_on lazy_depends_on].include?(option)
              namespaced_dependencies(value)
            else
              value
            end
        end
      end

      def namespaced_dependencies(declaration)
        case declaration
        when ::Array
          declaration.map { |entry|
            if entry.is_a?(::Array)
              [entry[0], namespaced(entry[1])]
            else
              [entry, namespaced(entry)]
            end
          }
        when ::Hash
          declaration.transform_values { |key| namespaced(key) }
        else
          declaration
        end
      end
    end
  end
end
