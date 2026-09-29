# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2022-2023, by Samuel Williams.
# Copyright, 2022, by Jeremy Evans.

module Rack
  module Session
    RACK_SESSION                        = 'rack.session'
    RACK_SESSION_OPTIONS                = 'rack.session.options'
    RACK_SESSION_UNPACKED_COOKIE_DATA   = 'rack.session.unpacked_cookie_data'
    RACK_SESSION_CHUNKED_WRITE          = 'rack.session.chunked_write'
    RACK_SESSION_CHUNKED_EXPIRE         = 'rack.session.chunked_expire'
  end
end
