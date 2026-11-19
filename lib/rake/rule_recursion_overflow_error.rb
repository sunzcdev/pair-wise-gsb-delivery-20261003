# frozen_string_literal: true
module Rake

  # Error indicating a recursion overflow error in task selection.
  #
  # When the failure comes from the bounded rule search, +report+ holds the
  # RuleSearchReport with every attempted rule and rejection reason.  The
  # target chain is rendered in the message followed by that diagnosis.
  class RuleRecursionOverflowError < StandardError
    # Optional RuleSearchReport describing the rule search that stopped.
    attr_accessor :report

    # Whether the target file itself exists on disk.  An undetermined plan
    # can still be about an existing file; that must not be read as "the
    # file is missing".
    attr_accessor :target_file_exists

    def initialize(*args)
      super
      @targets = []
      @target_file_exists = nil
    end

    def add_target(target)
      @targets << target
    end

    def targets
      @targets.dup
    end

    def message
      base = String.new(super)
      base << ": [" << @targets.reverse.join(" => ") << "]" unless @targets.empty?
      base << report.diagnostic_suffix if report
      base
    end
  end

end
