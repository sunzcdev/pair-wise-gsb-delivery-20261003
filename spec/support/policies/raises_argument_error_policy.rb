# frozen_string_literal: true

# A policy with a correct `(user, record)` signature that raises an
# ArgumentError from inside the constructor body, mimicking e.g.
# `Integer("bad")`. Pundit must let the original error propagate.
class RaisesArgumentErrorPolicy
  class << self
    def attempts
      @attempts || 0
    end

    attr_writer :attempts
  end

  def initialize(user, record)
    self.class.attempts += 1
    @user = user
    @record = record
    Integer("bad")
  end

  attr_reader :user, :record

  class Scope
    def initialize(user, scope)
      Integer("bad")
    end
  end
end
