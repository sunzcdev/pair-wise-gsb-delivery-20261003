# frozen_string_literal: true

class Post
  attr_accessor :archived

  def initialize(user = nil)
    @user = user
    @archived = false
  end

  attr_reader :user

  def self.published
    :published
  end

  def self.read
    :read
  end

  def to_s
    "Post"
  end
end
