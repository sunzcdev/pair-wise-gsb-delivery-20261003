# frozen_string_literal: true

class DocumentPolicy < BasePolicy
  class << self
    def reset_calls!
      @calls = Hash.new(0)
    end

    def calls
      @calls ||= Hash.new(0)
    end

    def count(query, object)
      calls[[query, object.object_id]] += 1
    end

    def call_count(query, object)
      calls[[query, object.object_id]]
    end
  end

  alias_method :document, :record

  def show?
    self.class.count(:show?, document)
    document.published
  end

  def edit?
    self.class.count(:edit?, document)
    document.owner == user
  end

  def archive?
    self.class.count(:archive?, document)
    document.owner == user && !document.archived
  end
end
