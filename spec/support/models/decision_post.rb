# frozen_string_literal: true

class DecisionPost
  def initialize(user = nil)
    @user = user
  end

  attr_reader :user

  def to_s
    "DecisionPost"
  end
end
