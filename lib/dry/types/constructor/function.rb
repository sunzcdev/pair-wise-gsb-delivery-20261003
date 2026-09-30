# frozen_string_literal: true

require "concurrent/map"

module Dry
  module Types
    class Constructor < Nominal
      # Function is used internally by Constructor types
      #
      # @api private
      class Function
        # Wrapper for unsafe coercion functions
        #
        # @api private
        class Safe < Function
          def call(input, &)
            @fn.(input)
          rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
            CoercionError.handle(exception, &)
          end
        end

        # Coercion function that explicitly receives the context passed
        # to a type call. The fallback block is forwarded so the callable
        # can +yield+ on a failed coercion.
        #
        # @api private
        class Contextual < Function
          def call(input, context = Undefined, &block)
            if block
              @fn.call(input, context, &block)
            else
              @fn.call(input, context)
            end
          end

          def contextual? = true
        end

        # Contextual counterpart of {Safe}
        #
        # @api private
        class SafeContextual < Function
          def call(input, context = Undefined, &)
            @fn.(input, context)
          rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
            CoercionError.handle(exception, &)
          end

          def contextual? = true
        end

        # Coercion via a method call on a known object
        #
        # @api private
        class MethodCall < Function
          @cache = ::Concurrent::Map.new

          # Marker module for method calls that receive the call context
          #
          # @api private
          module Contextual
            def contextual? = true
          end

          # Choose or build the base class
          #
          # @return [Function]
          def self.call_class(method, public, safe, contextual)
            @cache.fetch_or_store([method, public, safe, contextual]) do
              if contextual
                ContextualCall.call_class(method, public, safe)
              elsif public
                ::Class.new(PublicCall) do
                  include PublicCall.call_interface(method, safe)

                  define_method(:__to_s__) do
                    "#<PublicCall for :#{method}>"
                  end
                end
              elsif safe
                PrivateCall
              else
                PrivateSafeCall
              end
            end
          end

          # Coercion with a context-aware method call
          #
          # @api private
          class ContextualCall < MethodCall
            @interfaces = ::Concurrent::Map.new

            # Choose or build the interface
            #
            # @return [::Module]
            def self.call_interface(method, safe)
              @interfaces.fetch_or_store([method, safe]) do
                ::Module.new do
                  if safe
                    module_eval(<<~RUBY, __FILE__, __LINE__ + 1)
                      def call(input, context = Undefined, &block)  # def call(input, context, &block)
                        @target.#{method}(input, context, &block)   #   @target.coerce(input, context, &block)
                      end                                          # end
                    RUBY
                  else
                    module_eval(<<~RUBY, __FILE__, __LINE__ + 1)
                      def call(input, context = Undefined, &block)                 # def call(input, context, &block)
                        @target.#{method}(input, context, &block)                 #   @target.coerce(input, context, &block)
                      rescue ::NoMethodError, ::TypeError, ::ArgumentError => e # rescue ...
                        CoercionError.handle(e, &block)                          #   CoercionError.handle(e, &block)
                      end                                                        # end
                    RUBY
                  end
                end
              end
            end

            def self.call_class(method, public, safe)
              if public
                ::Class.new(self) do
                  include MethodCall::Contextual
                  include ContextualCall.call_interface(method, safe)

                  define_method(:__to_s__) do
                    "#<ContextualCall for :#{method}>"
                  end
                end
              elsif safe
                PrivateContextualCall
              else
                PrivateSafeContextualCall
              end
            end
          end

          # Coercion with a publicly accessible method call
          #
          # @api private
          class PublicCall < MethodCall
            @interfaces = ::Concurrent::Map.new

            # Choose or build the interface
            #
            # @return [::Module]
            def self.call_interface(method, safe)
              @interfaces.fetch_or_store([method, safe]) do
                ::Module.new do
                  if safe
                    module_eval(<<~RUBY, __FILE__, __LINE__ + 1)
                      def call(input, &)             # def call(input, &)
                        @target.#{method}(input, &)  #   @target.coerce(input, &)
                      end                            # end
                    RUBY
                  else
                    module_eval(<<~RUBY, __FILE__, __LINE__ + 1)
                      def call(input, &)                                             # def call(input, &)
                        @target.#{method}(input)                                     #   @target.coerce(input)
                      rescue ::NoMethodError, ::TypeError, ::ArgumentError => error  # rescue ::NoMethodError, ::TypeError, ::ArgumentError => error
                        CoercionError.handle(error, &)                               #   CoercionError.handle(error, &)
                      end                                                            # end
                    RUBY
                  end
                end
              end
            end
          end

          # Coercion via a private method call
          #
          # @api private
          class PrivateCall < MethodCall
            def call(input, &) = @target.send(@name, input, &)
          end

          # Coercion via an unsafe private method call
          #
          # @api private
          class PrivateSafeCall < PrivateCall
            def call(input, &)
              @target.send(@name, input)
            rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
              CoercionError.handle(exception, &)
            end
          end

          # Coercion via a context-aware private method call
          #
          # @api private
          class PrivateContextualCall < MethodCall
            include Contextual

            def call(input, context = Undefined, &block) =
              @target.send(@name, input, context, &block)
          end

          # Coercion via a context-aware unsafe private method call
          #
          # @api private
          class PrivateSafeContextualCall < PrivateContextualCall
            def call(input, context = Undefined, &block)
              @target.send(@name, input, context, &block)
            rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
              CoercionError.handle(exception, &block)
            end
          end

          # @api private
          #
          # @return [MethodCall]
          def self.[](fn, safe, contextual: false)
            public = fn.receiver.respond_to?(fn.name)
            MethodCall.call_class(fn.name, public, safe, contextual).new(fn)
          end

          attr_reader :target, :name

          def initialize(fn)
            super
            @target = fn.receiver
            @name = fn.name
          end

          def to_ast = [:method, target, name]
        end

        class Wrapper < Function
          # @return [Object]
          def call(input, type, &)
            @fn.(input, type, &)
          rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
            CoercionError.handle(exception, &)
          end
          alias_method :[], :call

          def arity = 2
        end

        # Wrapper function that also receives the context passed to
        # a type call, i.e. +fn.(input, type, context, &block)+
        #
        # @api private
        class ContextualWrapper < Function
          # @return [Object]
          def call(input, type, context = Undefined, &)
            @fn.(input, type, context, &)
          rescue ::NoMethodError, ::TypeError, ::ArgumentError => exception
            CoercionError.handle(exception, &)
          end
          alias_method :[], :call

          def arity = 3

          def wrapper? = true

          def contextual? = true
        end
        # Choose or build specialized invokation code for a callable
        #
        # @param [#call] fn
        # @param [Boolean] contextual whether the callable explicitly
        #   receives the context of a call
        # @return [Function]
        def self.[](fn, contextual: false)
          raise ::ArgumentError, "Missing constructor block" if fn.nil?

          if fn.is_a?(Function)
            fn
          elsif contextual && fn.respond_to?(:arity) && fn.arity.equal?(3)
            # A callable with (input, type, context) signature is a wrapper
            # that also receives the context of the call
            ContextualWrapper.new(fn)
          elsif fn.respond_to?(:arity) && fn.arity.equal?(2) && !contextual
            Wrapper.new(fn)
          elsif fn.is_a?(::Method)
            MethodCall[fn, yields_block?(fn), contextual: contextual]
          elsif contextual
            if yields_block?(fn)
              Contextual.new(fn)
            else
              SafeContextual.new(fn)
            end
          elsif yields_block?(fn)
            new(fn)
          else
            Safe.new(fn)
          end
        end

        # @return [Boolean]
        def self.yields_block?(fn)
          *, (last_arg,) =
            if fn.respond_to?(:parameters)
              fn.parameters
            else
              fn.method(:call).parameters
            end

          last_arg.equal?(:block)
        end

        include ::Dry::Equalizer(:fn, immutable: true)

        attr_reader :fn

        def initialize(fn)
          @fn = fn
        end

        # @return [Object]
        def call(input, &) = @fn.(input, &)
        alias_method :[], :call

        # @return [Integer]
        def arity = 1

        def wrapper? = arity.equal?(2)

        # Whether the function explicitly receives the context of a call
        #
        # @return [Boolean]
        def contextual? = false

        # @return [Array]
        def to_ast
          if fn.is_a?(::Proc)
            [:id, FnContainer.register(fn)]
          else
            [:callable, fn]
          end
        end

        # @return [Function]
        def >>(other)
          f = Function[other]

          if contextual? || f.contextual?
            pipeline = contextual_pipeline(self, f)

            if wrapper? || f.wrapper?
              ContextualWrapper.new(pipeline)
            else
              Contextual.new(pipeline)
            end
          else
            Function[-> x, &b { f.(self.(x, &b), &b) }]
          end
        end

        # @return [Function]
        def <<(other)
          f = Function[other]

          if contextual? || f.contextual?
            pipeline = contextual_pipeline(f, self)

            if wrapper? || f.wrapper?
              ContextualWrapper.new(pipeline)
            else
              Contextual.new(pipeline)
            end
          else
            Function[-> x, &b { self.(f.(x, &b), &b) }]
          end
        end

        private

        # Build a lambda applying +first+ then +second+. Each step receives
        # the call context (and the type, for wrapper functions) only when it
        # declares so. Wrapper composition is structural at the constructor
        # level, so wrapper steps encountered here are invoked with the type
        # passed through the pipeline.
        #
        # The lambda follows the +Contextual+ convention +(input, context)+;
        # when it wraps a wrapper step it is stored in a +ContextualWrapper+
        # and additionally receives the type as the second argument.
        #
        # @param [Function] first
        # @param [Function] second
        # @return [Proc]
        def contextual_pipeline(first, second)
          first_contextual = first.contextual?
          second_contextual = second.contextual?
          first_wrapper = first.wrapper?
          second_wrapper = second.wrapper?
          has_wrapper = first_wrapper || second_wrapper

          if has_wrapper
            lambda do |input, type, context = Undefined, &block|
              intermediate = apply_contextual_step(
                first, input, type, context, first_contextual, first_wrapper, block
              )

              apply_contextual_step(
                second, intermediate, type, context,
                second_contextual, second_wrapper, block
              )
            end
          else
            lambda do |input, context = Undefined, &block|
              intermediate = apply_contextual_step(
                first, input, nil, context, first_contextual, false, block
              )

              apply_contextual_step(
                second, intermediate, nil, context,
                second_contextual, false, block
              )
            end
          end
        end

        # Invoke a single step of a contextual pipeline, honoring whether it
        # is a wrapper and/or context-aware
        #
        # @api private
        def apply_contextual_step(step, value, type, context, contextual, wrapper, block)
          if wrapper && contextual
            step.(value, type, context, &block)
          elsif wrapper
            step.(value, type, &block)
          elsif contextual
            step.(value, context, &block)
          else
            step.(value, &block)
          end
        end
      end
    end
  end
end
