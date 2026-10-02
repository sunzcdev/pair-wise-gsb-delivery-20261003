# frozen_string_literal: true

module Dry
  # Shared errors
  #
  # @api public
  module Configurable
    extend Dry::Core::Deprecations["dry-configurable"]

    Error = Class.new(::StandardError)

    FrozenConfigError = Class.new(Error)

    # Raised when a config cannot be exported as a shareable snapshot due to a value that cannot
    # be safely copied across Ractor boundaries.
    UnsupportedValueError = Class.new(Error)

    AlreadyIncludedError = Class.new(Error)
    deprecate_constant(
      :AlreadyIncludedError,
      message: "`include Dry::Configurable` more than once (if needed)"
    )
  end
end
