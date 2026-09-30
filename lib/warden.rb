# encoding: utf-8
# frozen_string_literal: true
require 'forwardable'

require 'warden/mixins/common'
require 'warden/proxy'
require 'warden/manager'
require 'warden/errors'
require 'warden/session_serializer'
require 'warden/strategies'
require 'warden/strategies/base'

module Warden
  class NotAuthenticated < StandardError; end

  # Raised by a session deserializer when the identity store backing a
  # serialized session key is temporarily unavailable, so it is impossible
  # to tell whether the identity still exists or not.
  #
  # This must not be confused with a "missing identity" (the deserializer
  # returning nil): the latter means the account is gone and the session can
  # be removed, while the former requires the session to be preserved so it
  # can be restored on a later request.
  class IdentityStoreTemporarilyUnavailable < StandardError; end

  module Test
    autoload :WardenHelpers,  'warden/test/warden_helpers'
    autoload :Helpers,        'warden/test/helpers'
    autoload :Mock,        'warden/test/mock'
  end

  # Provides helper methods to warden for testing.
  #
  # To setup warden in test mode call the +test_mode!+ method on warden
  #
  # @example
  #   Warden.test_mode!
  #
  # This will provide a number of methods.
  # Warden.on_next_request(&blk) - captures a block which is yielded the warden proxy on the next request
  # Warden.test_reset! - removes any captured blocks that would have been executed on the next request
  #
  # Warden.test_reset! should be called in after blocks for rspec, or teardown methods for Test::Unit
  def self.test_mode!
    unless Warden::Test::WardenHelpers === Warden
      Warden.extend Warden::Test::WardenHelpers
      Warden::Manager.on_request do |proxy|
        unless proxy.asset_request?
          while blk = Warden._on_next_request.shift
            blk.call(proxy)
          end
        end
      end
    end
    true
  end
end
