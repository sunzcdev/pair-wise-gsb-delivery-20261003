# encoding: utf-8
# frozen_string_literal: true

# A stand-in for an application identity. Two instances are never `==` even
# when they represent the same identity, so all-of groups cannot rely on Ruby
# object equality: the application must provide the comparison itself.
class FakeSubject
  attr_reader :id

  def initialize(id)
    @id = id
  end
end
