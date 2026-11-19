# frozen_string_literal: true
module Rake

  # Raised when code executed while loading an imported file attempts a
  # change that the import transaction cannot record, and therefore can
  # neither publish nor roll back atomically.
  class UnsupportedImportOperation < StandardError
    def initialize(operation, target=nil)
      message = "cannot #{operation}"
      message << " (#{target})" if target
      message << " while an import is being loaded: the change is not a" \
        " recordable task, rule or dependency definition, so the import" \
        " transaction would be unable to roll it back"
      super(message)
    end
  end

  ##
  # A restricted, declarative transaction around the loading of a single
  # imported file.
  #
  # Only recordable definitions participate in the transaction: tasks,
  # rules, dependency declarations and descriptions made through the
  # Rake DSL.  If the imported file raises while loading, every
  # definition recorded since the transaction began is rolled back, so a
  # half-loaded import is never mistaken for a complete one.  If the
  # file loads successfully, all of its recorded definitions are
  # published at once.
  #
  # The transaction deliberately does NOT attempt to undo the external
  # side effects of arbitrary Ruby code (writing files, requiring
  # libraries, defining methods, ...).  Changes to Rake's own state that
  # cannot be recorded -- such as clearing tasks -- are rejected with
  # Rake::UnsupportedImportOperation when they are triggered.
  class ImportTransaction
    def initialize(application)
      @application = application
    end

    # Run +block+ inside the transaction.  Definitions made by the block
    # are published when the block returns normally, and rolled back if
    # the block raises.
    def transaction
      @application.begin_import_transaction
      snapshot = @application.snapshot_declarative_state
      begin
        yield
      rescue Exception
        @application.restore_declarative_state(snapshot)
        raise
      end
    ensure
      @application.end_import_transaction
    end
  end
end