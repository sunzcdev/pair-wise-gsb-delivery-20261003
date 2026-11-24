# frozen_string_literal: true

module Dry
  # Shared constants
  #
  # @api private
  module Configurable
    include Dry::Core::Constants

    # A patch action that explicitly pins a setting to a value. Unlike the two
    # action sentinels below, any plain value (including `nil`) expresses this
    # intent, so this only exists for symmetry and documentation.
    SET = :__dry_configurable_set__

    # A patch action that releases a local override so the setting is resolved
    # from its defined default again. The setting keeps following later default
    # changes. This is a no-op on a setting that was never explicitly written.
    INHERIT = :__dry_configurable_inherit__

    # A patch action that removes a local override and any cached value,
    # returning the setting to its pristine, default-following state.
    REMOVE_OVERRIDE = :__dry_configurable_remove_override__
  end
end
