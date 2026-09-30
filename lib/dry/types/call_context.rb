# frozen_string_literal: true

module Dry
  module Types
    # CallContext carries a per-call context object through a type
    # application without storing any state on type instances.
    #
    # The context is kept in fiber-local storage, so interleaved fibers
    # and concurrent threads each see their own value. The previous
    # value is always restored once the call finishes or raises, hence
    # nothing leaks into the type instances or across calls.
    #
    # @example
    #   amount = Dry::Types["integer"].constructor(context: true) do |input, table|
    #     input[:value] * table.fetch(input[:unit])
    #   end
    #
    #   amount.call_with_context({value: 5, unit: "USD"}, context: {"USD" => 100})
    #   # => 500
    #
    # @api public
    module CallContext
      KEY = :__dry_types_call_context__

      # The context of the currently running call, if any
      #
      # @return [Object, nil]
      #
      # @api public
      def self.current = ::Thread.current[KEY]

      # Whether a call context is set for the current fiber
      #
      # @return [Boolean]
      #
      # @api public
      def self.current? = !::Thread.current[KEY].nil?

      # Run a block with the given call context
      #
      # The previously set context (if any) is restored afterwards,
      # even when the block raises.
      #
      # @param [Object] context
      #
      # @return [Object] the block result
      #
      # @api public
      def self.with(context)
        previous = ::Thread.current[KEY]
        ::Thread.current[KEY] = context
        yield
      ensure
        ::Thread.current[KEY] = previous
      end
    end
  end
end
