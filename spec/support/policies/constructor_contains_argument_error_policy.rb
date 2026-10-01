# frozen_string_literal: true

class ConstructorContainsArgumentErrorPolicy
  def initialize(user, record)
    @user = user
    @record = record
    Integer("not a number")
  end

  class Scope
    def initialize(user, scope)
      @user = user
      @scope = scope
      Integer("not a number")
    end

    def resolve
      []
    end
  end
end
