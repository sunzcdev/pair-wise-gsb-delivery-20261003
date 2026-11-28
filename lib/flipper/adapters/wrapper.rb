module Flipper
  module Adapters
    # A base class for any adapter that wraps another adapter. By default, all methods
    # delegate to the wrapped adapter. Implement `#wrap` to customize the behavior of
    # all delegated methods, or override individual methods as needed.
    class Wrapper
      include Flipper::Adapter

      METHODS = [
        :import,
        :export,
        :features,
        :add,
        :remove,
        :clear,
        :get,
        :get_multi,
        :get_all,
        :enable,
        :disable,
        :sync_version,
        :sync_version=,
      ].freeze

      attr_reader :adapter

      def initialize(adapter)
        @adapter = adapter
      end

      METHODS.each do |method|
        if RUBY_VERSION >= '3.0'
          define_method(method) do |*args, **kwargs|
            wrap(method, *args, **kwargs) { @adapter.public_send(method, *args, **kwargs) }
          end
        else
          define_method(method) do |*args|
            wrap(method, *args) { @adapter.public_send(method, *args) }
          end
        end
      end

      # Wrapping an adapter doesn't make it writable.
      def read_only?
        @adapter.read_only?
      end

      # Public: Replaces one member of a set gate with another, delegating to
      # the wrapped adapter. Defined explicitly (rather than through METHODS)
      # because it takes a keyword argument that must reach the wrapped
      # adapter, and so ReadOnly can block it through #wrap.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        wrap(:replace, feature, gate, old_thing, new_thing, expected: expected) do
          @adapter.replace(feature, gate, old_thing, new_thing, expected: expected)
        end
      end

      # Override this method to customize the behavior of all delegated methods, and just yield to
      # the block to call the wrapped adapter.
      if RUBY_VERSION >= '3.0'
        def wrap(method, *args, **kwargs, &block)
          block.call
        end
      else
        def wrap(method, *args, &block)
          block.call
        end
      end
    end
  end
end
