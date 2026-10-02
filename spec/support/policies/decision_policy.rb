# frozen_string_literal: true

class DecisionPolicy < BasePolicy
  def show?
    Pundit::Decision.allow
  end

  def update?
    Pundit::Decision.deny(code: :not_owner, details: {owner_id: record.user.object_id})
  end

  def destroy?
    false
  end
end
