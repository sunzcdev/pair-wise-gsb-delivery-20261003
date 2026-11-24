# frozen_string_literal: true

module Dry
  # Shared errors
  #
  # @api public
  module Configurable
    extend Dry::Core::Deprecations["dry-configurable"]

    Error = Class.new(::StandardError)

    FrozenConfigError = Class.new(Error)

    # Base class for errors raised while validating or applying a {Patch}.
    PatchError = Class.new(Error)

    # Raised when a patch references a path that does not map to a defined setting.
    InvalidPatchPathError = Class.new(PatchError)

    # Raised when a patch carries a value that a setting's constructor rejects,
    # or when an action cannot be performed on its target setting.
    InvalidPatchValueError = Class.new(PatchError)

    AlreadyIncludedError = Class.new(Error)
    deprecate_constant(
      :AlreadyIncludedError,
      message: "`include Dry::Configurable` more than once (if needed)"
    )
  end
end
