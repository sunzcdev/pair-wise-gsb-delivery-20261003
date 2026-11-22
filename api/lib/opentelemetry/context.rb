# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'opentelemetry/context/key'
require 'opentelemetry/context/propagation'

module OpenTelemetry # rubocop:disable Style/Documentation
  Fiber.attr_accessor :opentelemetry_context

  # Manages context on a per-fiber basis
  class Context
    EMPTY_ENTRIES = {}.freeze
    private_constant :EMPTY_ENTRIES

    DetachError = Class.new(OpenTelemetry::Error) # rubocop:disable Style/EmptyClassDefinition

    # Token returned by {Context.attach} and consumed by the matching
    # {Context.detach}. It records the Fiber that performed the attach and the
    # depth of the context stack at that point, so that a detach can detect
    # both an ownership mismatch (the token belongs to another Fiber) and a
    # stack ordering mismatch (an outer context is being detached while inner
    # contexts are still active).
    class Token
      # @return [Fiber] The Fiber that performed the matching attach.
      attr_reader :fiber

      # @return [Integer] The size of the owning Fiber's context stack after
      #   the matching attach.
      attr_reader :stack_depth

      # @api private
      def initialize(fiber, stack_depth)
        @fiber = fiber
        @stack_depth = stack_depth
      end
    end

    class << self
      # Returns a key used to index a value in a Context
      #
      # @param [String] name The key name
      # @return [Context::Key]
      def create_key(name)
        Key.new(name)
      end

      # Returns current context, which is never nil
      #
      # @return [Context]
      def current
        stack.last || ROOT
      end

      # Associates a Context with the caller's current Fiber. Every call to
      # this operation should be paired with a corresponding call to detach.
      #
      # Returns a token to be used with the matching call to detach. The token
      # is bound to the calling Fiber: detaching it from a different Fiber (for
      # example, when a task object resumes on another Fiber) reports a
      # DetachError through the configured error handler without disturbing
      # either Fiber's context stack.
      #
      # @param [Context] context The new context
      # @return [Token] A token to be used when detaching
      def attach(context)
        s = stack
        s.push(context)
        Token.new(Fiber.current, s.size)
      end

      # Restores the previous Context associated with the current Fiber.
      # The supplied token is used to check if the call to detach is balanced
      # with a corresponding attach call. A {DetachError} is reported via the
      # configured error handler when:
      #
      # 1. the token was issued by a different Fiber (an ownership mismatch),
      #    or
      # 2. the token does not correspond to the top of the current Fiber's
      #    context stack (a stack ordering mismatch, such as detaching an
      #    outer context before its inner contexts).
      #
      # On a mismatch the current Fiber's context stack is left untouched, so
      # the Fiber that owns the token can still resume and perform its own
      # balanced detach instead of losing a stack layer to the failed call.
      #
      # @param [Token] token The token provided by the matching call to attach
      # @return [Boolean] True if the calls matched, false otherwise
      def detach(token)
        s = stack

        unless token.is_a?(Token)
          OpenTelemetry.handle_error(exception: DetachError.new("calls to detach should match corresponding calls to attach. Expected a Token returned by attach, got #{token.class}."))
          return false
        end

        unless token.fiber.equal?(Fiber.current)
          OpenTelemetry.handle_error(exception: DetachError.new('calls to detach should match corresponding calls to attach. The token was issued by a different fiber.'))
          return false
        end

        unless token.stack_depth == s.size
          OpenTelemetry.handle_error(exception: DetachError.new('calls to detach should match corresponding calls to attach. The token does not match the top of the current fiber context stack.'))
          return false
        end

        s.pop
        true
      end

      # Executes a block with ctx as the current context. It restores
      # the previous context upon exiting.
      #
      # @param [Context] ctx The context to be made active
      # @yield [context] Yields context to the block
      # @yieldparam [Context] context The active context
      def with_current(ctx)
        token = attach(ctx)
        yield ctx
      ensure
        detach(token)
      end

      # Execute a block in a new context with key set to value. Restores the
      # previous context after the block executes.

      # @param [String] key The lookup key
      # @param [Object] value The object stored under key
      # @yield [context, value] Yields the newly created context and value to
      #   the block
      # @yieldparam [Context] context The newly created context
      # @yieldparam [Object] value The object stored under key
      def with_value(key, value)
        ctx = current.set_value(key, value)
        token = attach(ctx)
        yield ctx, value
      ensure
        detach(token)
      end

      # Execute a block in a new context where its values are merged with the
      # incoming values. Restores the previous context after the block executes.

      # @param [Hash] values Will be merged with values of the current context
      #  and returned in a new context
      # @yield [context, values] Yields the newly created context and values
      #   to the block
      # @yieldparam [Context] context The newly created context
      # @yieldparam [Hash] values The values merged into the new context
      def with_values(values)
        ctx = current.set_values(values)
        token = attach(ctx)
        yield ctx, values
      ensure
        detach(token)
      end

      # Returns the value associated with key in the current context
      #
      # @param [String] key The lookup key
      def value(key)
        current.value(key)
      end

      # Clears the fiber-local Context stack.
      def clear
        Fiber.current.opentelemetry_context = []
      end

      # Returns an empty context.
      #
      # @return [Context]
      def empty
        new(EMPTY_ENTRIES)
      end

      private

      def stack
        Fiber.current.opentelemetry_context ||= []
      end
    end

    def initialize(entries)
      @entries = entries.freeze
    end

    # Returns the corresponding value (or nil) for key
    #
    # @param [Key] key The lookup key
    # @return [Object]
    def value(key)
      @entries[key]
    end

    alias [] value

    # Returns a new Context where entries contains the newly added key and value
    #
    # @param [Key] key The key to store this value under
    # @param [Object] value Object to be stored under key
    # @return [Context]
    def set_value(key, value)
      new_entries = @entries.dup
      new_entries[key] = value
      Context.new(new_entries)
    end

    # Returns a new Context with the current context's entries merged with the
    #   new entries
    #
    # @param [Hash] values The values to be merged with the current context's
    #   entries.
    # @return [Context]
    def set_values(values) # rubocop:disable Naming/AccessorMethodName
      Context.new(@entries.merge(values))
    end

    ROOT = empty.freeze
  end
end
