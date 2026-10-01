# frozen_string_literal: true

class InvalidConstructorPolicy
  def initialize(user, record, extra, optional = nil, *rest, keyword:)
    @user = user
    @record = record
    @extra = extra
    @optional = optional
    @rest = rest
    @keyword = keyword
  end

  class Scope
    def initialize(user, scope, extra, optional = nil, *rest, keyword:)
      @user = user
      @scope = scope
      @extra = extra
      @optional = optional
      @rest = rest
      @keyword = keyword
    end
  end
end
