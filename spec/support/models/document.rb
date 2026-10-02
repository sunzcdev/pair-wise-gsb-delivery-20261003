# frozen_string_literal: true

# A model whose equality is based on its id, mimicking Active Record: two
# different instances with the same id compare equal with `==`/`eql?`/`hash`,
# but are still distinct objects.
class Document
  def initialize(id:, published: true, archived: false, owner: nil)
    @id = id
    @published = published
    @archived = archived
    @owner = owner
  end

  attr_reader :id
  attr_accessor :published, :archived, :owner

  def ==(other)
    other.is_a?(Document) && other.id == id
  end
  alias_method :eql?, :==

  def hash
    [self.class, id].hash
  end
end
