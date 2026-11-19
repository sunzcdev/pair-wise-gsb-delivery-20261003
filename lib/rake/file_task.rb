# frozen_string_literal: true
require_relative "task"
require_relative "early_time"
require_relative "artifact"

module Rake

  # A FileTask is a task that includes time based dependencies.  If any of a
  # FileTask's prerequisites have a timestamp that is later than the file
  # represented by this task, then the file must be rebuilt (using the
  # supplied actions).
  #
  # == Controlled artifacts
  #
  # A file task normally trusts its actions to write the target file.  If an
  # action creates the target and then raises (or leaves a truncated file
  # behind), the file exists and a later invocation may treat the task as up
  # to date even though the output is incomplete.
  #
  # Declaring the output as a controlled artifact changes where the action
  # writes and when the final name appears:
  #
  #   file "build/report.json" => ["src/report.json"] do |t|
  #     art = t.artifacts(validate: { "build/report.json" => ->(path) {
  #       JSON.parse(File.read(path))["complete"]
  #     } })
  #
  #     art.write("build/report.json") do |io|
  #       io.write(build_report)
  #     end
  #   end
  #
  # The action only ever writes a staging file.  After the action returns,
  # Rake runs the declared checks against that staging file and only then
  # renames it onto the final output.  Until that rename the previous target,
  # including its timestamp, is left alone, so a failed build keeps the
  # original output and the next invocation cannot mistake a partial file for
  # a finished one.
  #
  # Each declared output is validated and published on its own, in declaration
  # order.  This is a per-output publication guarantee, not a multi-file
  # transaction; see doc/artifacts.rdoc.
  class FileTask < Task

    # The Rake::ArtifactSet declared for this task, or nil.  It exists from the
    # moment #artifacts is called, so an action can stage its outputs through
    # it.  A fresh build resets the set before the actions run.
    attr_reader :artifact_set

    # True when this task declared controlled artifacts.
    def artifacts?
      !@artifact_set.nil?
    end

    # Declare the outputs of this task as controlled artifacts and return the
    # Rake::ArtifactSet the action should write through.
    #
    # With no +outputs+ the task's own name is the only output, so
    # <tt>t.artifacts</tt> is enough for a single-file task; any existing
    # declaration is reused.  Passing +outputs+ replaces the declaration and
    # must include the task name, because that name is what Rake compares
    # against timestamps and prerequisites:
    #
    #   t.artifacts("report.json", "report.json.sig")
    #
    # +validate:+ turns the checks into part of the declaration.  It accepts
    # either one callable applied to every output or a Hash mapping an output
    # name to its callable.  A check runs against the written staging file and
    # accepts it by returning a truthy value; returning false or raising
    # rejects the artifact.  A check is a validation, never a build: it may
    # not run the task again and must not publish anything itself.
    #
    # An optional block receives the set, which is convenient for declaring
    # the staging writes together with the checks at the call site:
    #
    #   t.artifacts(validate: { "a.txt" => ->(path) { File.size(path) > 0 } })
    #
    #   t.artifacts do |art|
    #     art.write("a.txt") { |io| io.write(payload) }
    #   end
    def artifacts(*outputs, validate: nil)
      set =
        if outputs.empty?
          @artifact_set || build_artifact_set([name])
        else
          build_artifact_set(outputs)
        end
      set.declare_validations(validate)
      yield set if block_given?
      set
    end

    # Is this file task needed?  Yes if it doesn't exist, or if its time stamp
    # is out of date.  A task that declares controlled artifacts is also
    # needed when any declared output is missing, so a partially published
    # multi-output task is never mistaken for a finished one.
    def needed?
      return true if artifacts? && artifact_outputs_missing?
      begin
        out_of_date?(File.mtime(name)) || @application.options.build_all
      rescue Errno::ENOENT
        true
      end
    end

    # Time stamp for file task.
    def timestamp
      begin
        File.mtime(name)
      rescue Errno::ENOENT
        Rake::LATE
      end
    end

    # Run the task actions and then publish the declared artifacts.
    #
    # When the task declares artifacts (or an action declares them) the action
    # only writes staging files.  Rake validates each staged file after the
    # actions return and renames it onto its final name.  A failure anywhere
    # leaves the previous outputs untouched, deletes the staging files and
    # deletes any new final output the action wrote directly, so the next
    # invocation cannot mistake a partial build for a finished one.  Nothing
    # here adjusts timestamps.
    def execute(args=nil)
      return super if application.options.dryrun

      if @publishing_artifacts
        raise Rake::ArtifactError,
              "artifact validation of #{name.inspect} must not run the task again; " \
              "validators check the staged output and never rebuild it"
      end

      @artifact_set.reset! if @artifact_set

      begin
        result = super
        publish_artifacts! if @artifact_set
        result
      rescue Exception => ex
        raise artifact_failure(ex)
      end
    end

    private

    # Declare a new artifact set for this task, rejecting output lists that
    # omit the task name.
    def build_artifact_set(outputs)
      set = Rake::ArtifactSet.new(outputs, task: self)
      unless set.outputs.include?(name)
        raise ArgumentError,
              "the artifact outputs #{set.outputs.inspect} of task #{name.inspect} " \
              "must include the task name"
      end
      @artifact_set = set
    end

    # True when any declared artifact output is absent.  Only real files
    # count, so a directory left at an output name does not satisfy the task.
    def artifact_outputs_missing?
      @artifact_set.outputs.any? { |output| !File.file?(output) }
    end

    # Are there any prerequisites with a later time than the given time stamp?
    def out_of_date?(stamp)
      all_prerequisite_tasks.any? { |prereq|
        prereq_task = application[prereq, @scope]
        if prereq_task.instance_of?(Rake::FileTask)
          prereq_task.timestamp > stamp || @application.options.build_all
        else
          prereq_task.timestamp > stamp
        end
      }
    end

    # Run the declared validations and publish every declared output.  The
    # guard makes a validator that calls Task#invoke again fail loudly instead
    # of silently rebuilding the artifact it is supposed to check.
    def publish_artifacts!
      return if @artifact_set.published?
      @publishing_artifacts = true
      @artifact_set.publish!
    ensure
      @publishing_artifacts = false
    end

    # Return the exception to raise after a failed artifact build.  Staged
    # files and unpublished new outputs are removed; if that removal fails the
    # leftover paths are reported through Rake::ArtifactCleanupError instead
    # of being swallowed.  The failure that triggered the cleanup stays
    # available as the returned error's +cause+.
    def artifact_failure(original)
      set = @artifact_set
      return original unless set

      leftovers = set.cleanup!
      leftovers += set.remove_unpublished_new_outputs!
      leftovers.uniq!
      return original if leftovers.empty?

      Rake::ArtifactCleanupError.new(
        "artifact task #{name.inspect} failed with " \
        "#{original.class}: #{original.message}; could not remove " \
        "#{leftovers.size} leftover file(s): #{leftovers.join(', ')}",
        leftovers: leftovers
      )
    end

    # ----------------------------------------------------------------
    # Task class methods.
    #
    class << self
      # Apply the scope to the task name according to the rules for this kind
      # of task.  File based tasks ignore the scope when creating the name.
      def scope_name(scope, task_name)
        Rake.from_pathname(task_name)
      end
    end
  end
end
