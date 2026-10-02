# frozen_string_literal: true

class ExplosionPolicy < BasePolicy
  def show?
    raise "boom"
  end
end
