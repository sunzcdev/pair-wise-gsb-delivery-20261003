# frozen_string_literal: true

# Pure in-memory fake resource. Every lifecycle event (acquire, use,
# release) is appended to a shared log so specs can assert the exact
# ordering across nested resources. Kept outside the Test module, which
# has its constants removed after every example.
class FakeResource
  attr_reader :name

  def initialize(name, log, release_error: nil)
    @name = name
    @log = log
    @release_error = release_error
    @log << [:acquire, name]
  end

  def use(label = :work)
    @log << [:use, name, label]
    self
  end

  def release
    @log << [:release, name]
    raise @release_error if @release_error
  end
end
