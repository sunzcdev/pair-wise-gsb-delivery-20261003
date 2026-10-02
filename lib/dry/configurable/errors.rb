# frozen_string_literal: true

module Dry
  # Shared errors
  #
  # @api public
  module Configurable
    extend Dry::Core::Deprecations["dry-configurable"]

    Error = Class.new(::StandardError)

    FrozenConfigError = Class.new(Error)

    # Raised when a config value cannot be included in a Ractor-shareable snapshot
    #
    # @api public
    UnsupportedValueError = Class.new(Error)

    AlreadyIncludedError = Class.new(Error)
    deprecate_constant(
      :AlreadyIncludedError,
      message: "`include Dry::Configurable` more than once (if needed)"
    )
  end
end
