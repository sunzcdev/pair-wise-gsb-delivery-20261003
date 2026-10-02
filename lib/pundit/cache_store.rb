# frozen_string_literal: true

module Pundit
  # Namespace for cache store implementations.
  #
  # Cache stores are used to cache policy lookups, so you get the same policy
  # instance for the same record.
  # @since v2.3.2
  module CacheStore
    # @!group Cache Store Interface

    # @!method fetch(user:, record:, namespaces: nil, &block)
    #   Looks up a stored policy or generate a new one.
    #
    #   @since v2.3.2
    #   @note This is a method template, but the method does not exist in this module.
    #   @note `namespaces` is only passed when the context is configured with
    #     policy namespaces. Cache stores must treat different namespace
    #     chains as different cache entries.
    #   @param user [Object]  the user that initiated the action
    #   @param record [Object] the object being accessed
    #   @param namespaces [Array<Module, #to_s>, nil] the policy namespace
    #     lookup chain of the context
    #   @param block [Proc] the block to execute if missing
    #   @return [Object] the policy

    # @!endgroup
  end
end
