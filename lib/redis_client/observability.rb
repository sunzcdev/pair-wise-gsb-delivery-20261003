# frozen_string_literal: true

require "securerandom"
require "singleton"

require "redis_client/observability/redaction"
require "redis_client/observability/event"
require "redis_client/observability/observer"
require "redis_client/observability/instrumenter"

class RedisClient
  module Observability
  end
end
