# frozen_string_literal: true

module Dry
  module Validation
    # Configuration for contracts
    #
    # @see Contract#config
    #
    # @api public
    class Config < Schema::Config
      setting :macros, default: Macros::Container.new, constructor: :dup.to_proc

      # When enabled, each rule runs against an isolated copy of the context
      # and its writes are merged back only if the rule succeeds
      #
      # @api public
      setting :transactional_context, default: false

      # @api private
      def dup
        config = super
        config.macros = macros.dup
        config
      end
    end
  end
end
