# frozen_string_literal: true

# A policy whose constructor does not accept Pundit's `(user, record)`
# calling convention. It counts instantiation attempts so tests can assert
# that Pundit never actually calls the constructor.
class BadArityPolicy
  class << self
    def attempts
      @attempts || 0
    end

    attr_writer :attempts
  end

  def initialize(user)
    self.class.attempts += 1
    @user = user
  end
end
