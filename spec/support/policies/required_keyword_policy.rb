# frozen_string_literal: true

# A policy whose constructor requires a keyword argument that Pundit
# does not provide.
class RequiredKeywordPolicy
  def initialize(user, record, required:)
    @user = user
    @record = record
    @required = required
  end
end
