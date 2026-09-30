require 'digest'

module Dentaku
  module AST
    class FunctionRegistry < Hash
      def initialize
        super
        @generation = 0
      end

      # monotonic stamp of the effective registry: own registrations plus
      # the global default registry this one falls back to, so a global
      # registration also invalidates instance-level compiled results
      def generation
        default.equal?(self) ? @generation : @generation + default.generation
      end

      # content identity of the effective registry: every function name
      # resolvable through this registry (instance entries layered over the
      # global defaults) mapped to the exact class implementing it, so
      # re-registration, instance overrides of global functions, and two
      # calculators with same-named but distinct implementations all
      # produce different fingerprints
      def fingerprint
        current = generation
        if @fingerprint.nil? || @fingerprint_generation != current
          entries = effective_entries.map { |name, function| "#{name}:#{function.object_id}" }
          @fingerprint = Digest::SHA256.hexdigest(entries.sort.join(';'))
          @fingerprint_generation = current
        end
        @fingerprint
      end

      def get(name)
        name = function_name(name)
        return self[name] if has_key?(name)
        return default[name] if default.has_key?(name)
        nil
      end

      def register(name, type, implementation, callback = nil, volatile: false)
        function = Class.new(Function) do
          def self.name=(name)
            @name = name.to_s
          end

          def self.name
            @name
          end

          # only the first registration of a given name claims the constant
          # under Dentaku::AST::Function, so later ones stay anonymous and
          # would otherwise render as "#<Class:0x...>" in error messages
          def self.display_name=(display_name)
            @display_name = display_name
          end

          def self.to_s
            @display_name || super
          end

          def self.inspect
            to_s
          end

          def self.implementation=(impl)
            @implementation = impl
          end

          def self.implementation
            @implementation
          end

          def self.type=(type)
            @type = type
          end

          def self.type
            @type
          end

          def self.callback=(callback)
            @callback = callback
          end

          def self.callback
            @callback
          end

          def self.volatile=(volatile)
            @volatile = volatile
          end

          def self.volatile?
            @volatile
          end

          def self.arity
            @implementation.arity < 0 ? nil : @implementation.arity
          end

          def self.min_param_count
            @implementation.parameters.select { |type, _name| type == :req }.count
          end

          def self.max_param_count
            @implementation.parameters.select { |type, _name| type == :rest }.any? ? Float::INFINITY : @implementation.parameters.count
          end

          def value(context = {})
            args = @args.map { |a| a.value(context) }
            self.class.implementation.call(*args)
          end

          def type
            self.class.type
          end
        end

        function.name = name
        function.display_name = "#{Function}::#{normalize_name(name)}"

        define_class(name, function)

        function.type = type
        function.implementation = implementation
        function.callback = callback
        function.volatile = volatile

        self[function_name(name)] = function
        @generation += 1
        function
      end

      def register_class(name, function_class)
        self[function_name(name)] = function_class
        @generation += 1
      end

      def default
        self.class.default
      end

      def self.default
        Dentaku::AST::Function.registry
      end

      protected

      def effective_entries
        default.equal?(self) ? self : default.effective_entries.merge(self)
      end

      private

      def function_name(name)
        name.to_s.downcase
      end

      def normalize_name(function_name)
        function_name.to_s.capitalize.gsub(/\W/, '_')
      end

      def define_class(function_name, function)
        class_name = normalize_name(function_name)
        return if Dentaku::AST::Function.const_defined?(class_name)

        Dentaku::AST::Function.const_set(class_name, function)
      end
    end
  end
end
