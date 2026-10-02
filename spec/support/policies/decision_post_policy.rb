# frozen_string_literal: true

class DecisionPostPolicy < BasePolicy
  alias_method :post, :record

  def show?
    Pundit::Decision.allow(code: :public, details: { owner: post.user })
  end

  def update?
    if post.user == user
      Pundit::Decision.allow(code: :owner)
    else
      Pundit::Decision.deny(code: :not_owner, details: { owner_id: post.user&.object_id })
    end
  end

  def destroy?
    Pundit::Decision.deny(code: :never_allowed, details: { secret: "do not log me" })
  end

  def archive?
    false
  end
end
