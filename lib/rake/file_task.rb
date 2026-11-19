# frozen_string_literal: true
require_relative "task"
require_relative "early_time"
require_relative "fingerprint_manifest"

module Rake

  # A FileTask is a task that includes time based dependencies.  If any of a
  # FileTask's prerequisites have a timestamp that is later than the file
  # represented by this task, then the file must be rebuilt (using the
  # supplied actions).
  #
  # File tasks can additionally opt into content fingerprint mode.  In that
  # mode freshness is decided from a reusable manifest that records a digest
  # of the task's declared inputs, a digest of its declared build parameters
  # and the identity of its successfully produced output.  See
  # Rake::FingerprintManifest for the manifest format.  Timestamp mode stays
  # the default and can still be selected per task with
  # <tt>fingerprint: false</tt> or globally with <tt>--no-fingerprint</tt>.
  #
  # Only the paths declared by a task are digested.  The workspace is never
  # scanned, and build parameter values (which may come from the environment)
  # are hashed rather than stored.
  #
  class FileTask < Task

    # Is this file task needed?
    #
    # In timestamp mode (the default) the task is needed when the output does
    # not exist or when a prerequisite has a later time stamp.
    #
    # In fingerprint mode the task is needed when the manifest holds no
    # successful entry for it, a declared (or previously recorded) input
    # disappeared or changed content, the declared build parameters changed,
    # or the current output no longer matches the recorded output identity.
    # A matching fingerprint wins over timestamps, which is what makes same
    # timestamp content replacements visible.  Prerequisites still run in
    # order, but only recorded inputs decide freshness.
    def needed?
      if build_all?
        @fingerprint_status = { reason: :build_all } if fingerprint_mode?
        return true
      end

      if fingerprint_mode?
        @fingerprint_status = fingerprint_check
        reason = @fingerprint_status[:reason]
        fingerprint_trace(@fingerprint_status) if reason
        return !reason.nil?
      end

      @fingerprint_status = nil
      begin
        out_of_date?(File.mtime(name))
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

    # Does this task use content fingerprints instead of timestamps?
    #
    # An explicit +fingerprint:+ setting on the task wins, then the
    # application option (<tt>--fingerprint</tt> / <tt>--no-fingerprint</tt>),
    # then the class default.
    def fingerprint_mode?
      return @fingerprint_mode unless @fingerprint_mode.nil?

      options = application_options
      if options && options.respond_to?(:fingerprint) && !options.fingerprint.nil?
        return options.fingerprint
      end

      self.class.fingerprint_mode
    end

    # Override the fingerprint mode for this task.  A value of nil restores
    # the default resolution (application option, then class default).
    attr_writer :fingerprint_mode

    # Declared input paths.  When nil, the file prerequisites (sources) of
    # the task are used.  Only declared paths are digested; nothing else in
    # the workspace is inspected.
    attr_accessor :fingerprint_inputs

    # Declared build parameters.  A Proc is evaluated whenever the
    # fingerprint is computed.  Only a digest is persisted.
    attr_accessor :fingerprint_parameters

    # Path of the reusable manifest.  When nil the application option or the
    # class default is used.
    attr_writer :fingerprint_manifest_path

    # Result of the most recent fingerprint check performed by #needed?.
    # A Hash with a :reason key (nil when the fingerprint matches) and an
    # optional :detail value.
    attr_reader :fingerprint_status

    # Resolve the manifest path for this task.
    def fingerprint_manifest_path
      return @fingerprint_manifest_path if @fingerprint_manifest_path

      options = application_options
      if options && options.respond_to?(:fingerprint_manifest) &&
         options.fingerprint_manifest
        return options.fingerprint_manifest
      end

      self.class.fingerprint_manifest_path
    end

    # The manifest object used by this task.
    def fingerprint_manifest
      path = fingerprint_manifest_path
      expanded = File.expand_path(path)
      if @fingerprint_manifest.nil? || @fingerprint_manifest.path != expanded
        @fingerprint_manifest = Rake::FingerprintManifest.for(path)
      end
      @fingerprint_manifest
    end

    # Configure fingerprint mode from the value given to the +file+ DSL:
    # +true+, +false+ or a Hash with :inputs, :parameters and :manifest keys.
    def configure_fingerprint(options)
      case options
      when nil
        self.fingerprint_mode = nil
      when Hash
        options = options.dup
        mode = options.delete(:mode)
        self.fingerprint_mode = mode.nil? ? true : mode
        self.fingerprint_inputs = options.delete(:inputs) if options.key?(:inputs)
        self.fingerprint_parameters = options.delete(:parameters) if options.key?(:parameters)
        self.fingerprint_manifest_path = options.delete(:manifest) if options.key?(:manifest)
        unless options.empty?
          fail ArgumentError,
            "Unknown fingerprint option(s): #{options.keys.map(&:inspect).join(', ')}"
        end
      else
        self.fingerprint_mode = !!options
      end
      self
    end

    # Check the recorded fingerprint against the current declared scope.
    #
    # Returns a Hash whose :reason is nil when the recorded fingerprint
    # matches, or one of:
    #
    # [:missing_output]     the output file does not exist
    # [:no_fingerprint]     there is no usable successful entry
    # [:missing_input]      a declared input is gone
    # [:scope_changed]      the set of declared inputs changed
    # [:input_changed]      a declared input has different content
    # [:parameters_changed] the declared build parameters changed
    # [:output_changed]     the output was replaced outside of a build
    def fingerprint_check
      return { reason: :missing_output, detail: name } unless File.exist?(name)

      entry = fingerprint_manifest.entry(name)
      return { reason: :no_fingerprint } unless entry

      unless entry.is_a?(Hash) &&
             entry["inputs"].is_a?(Hash) &&
             entry["output"].is_a?(Hash)
        return { reason: :no_fingerprint }
      end

      declared = declared_fingerprint_inputs.sort
      recorded_inputs = entry["inputs"]
      digests = {}
      checkable_fingerprint_inputs(recorded_inputs).each do |input|
        digest = Rake::FingerprintManifest.digest_file(input)
        return { reason: :missing_input, detail: input } if digest.nil?
        digests[input] = digest
      end

      unless digests == recorded_inputs
        changed = (digests.keys | recorded_inputs.keys).
          select { |path| digests[path] != recorded_inputs[path] }.
          sort
        return { reason: :input_changed, detail: changed }
      end

      # The declared scope may have shrunk since the recorded build.  A
      # stale success must not hide a source that is no longer declared.
      scope = entry["scope"]
      if scope.is_a?(Array) && scope != declared
        return { reason: :scope_changed, detail: declared }
      end

      unless current_fingerprint_parameters_digest == entry["parameters"]
        return { reason: :parameters_changed }
      end

      output = Rake::FingerprintManifest.digest_file(name)
      unless output && entry["output"]["digest"] == output
        return { reason: :output_changed, detail: name }
      end

      { reason: nil }
    end

    # Execute the actions of the task.
    #
    # In fingerprint mode the previous successful entry is dropped before
    # the actions run and a new entry is recorded only if they complete
    # successfully, so a failed action never leaves a success fingerprint
    # behind.
    def execute(args=nil)
      return super unless fingerprint_mode?
      return super if fingerprint_dry_run?

      # Digest the declared inputs before running the actions: a build may
      # consume or delete its sources.  A nil set means an input could not be
      # read, so no success may be recorded.
      declared = declared_fingerprint_inputs
      inputs = digest_fingerprint_inputs(declared)
      fingerprint_manifest.delete(name)
      result = super
      # An empty set means no input was visible yet (a rule can be applied
      # inside Task#execute), so look again now that the action has run.
      if inputs && inputs.empty?
        declared = declared_fingerprint_inputs
        inputs = digest_fingerprint_inputs(declared)
      end
      record_fingerprint(inputs, declared)
      result
    end

    private

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

    def build_all?
      options = application_options
      !!(options && options.respond_to?(:build_all) && options.build_all)
    end

    def fingerprint_dry_run?
      options = application_options
      !!(options && options.respond_to?(:dryrun) && options.dryrun)
    end

    def application_options
      application.options if application.respond_to?(:options)
    end

    def current_fingerprint_parameters
      parameters = fingerprint_parameters
      parameters.respond_to?(:call) ? parameters.call : parameters
    end

    def current_fingerprint_parameters_digest
      Rake::FingerprintManifest.digest_parameters(current_fingerprint_parameters)
    end

    # Inputs declared by this task.  Explicit declarations win; otherwise
    # the prerequisites that resolve to file tasks count as inputs.  Plain
    # task prerequisites (and directories) have no file content to digest
    # and are skipped.
    def declared_fingerprint_inputs
      if @fingerprint_inputs
        @fingerprint_inputs.map(&:to_s)
      else
        sources.map(&:to_s).select { |path| fingerprint_input_candidate?(path) }
      end
    end

    # Declared inputs plus every path recorded by the previous successful
    # build, so a source that disappeared is still explicitly invalidating.
    def checkable_fingerprint_inputs(recorded_inputs)
      (declared_fingerprint_inputs + recorded_inputs.keys).uniq
    end

    def fingerprint_input_candidate?(path)
      return false if File.directory?(path)
      return true if File.file?(path) && !plain_prerequisite_task?(path)

      fingerprint_file_prerequisite?(path)
    end

    # A path that names a non-file task is a task dependency, not a file
    # input, even if a file with the same name happens to exist.
    def plain_prerequisite_task?(path)
      return false unless application.respond_to?(:lookup)

      task = application.lookup(path, scope)
      task && !task.equal?(self) && !task.is_a?(Rake::FileTask)
    rescue StandardError
      false
    end

    def fingerprint_file_prerequisite?(path)
      return false unless application.respond_to?(:lookup)

      task = application.lookup(path, scope)
      task && !task.equal?(self) && task.is_a?(Rake::FileTask)
    rescue StandardError
      false
    end

    # Digest the given declared inputs.  Returns nil when any input cannot be
    # digested, so a success fingerprint is never recorded for a build whose
    # sources could not be read.
    def digest_fingerprint_inputs(inputs)
      digests = {}
      inputs.each do |path|
        digest = Rake::FingerprintManifest.digest_file(path)
        return nil if digest.nil?
        digests[path] = digest
      end
      digests
    end

    # Record a successful build for the given input digests.  Nothing is
    # recorded when the output cannot be digested, which keeps the task
    # needed on the next run instead of claiming a false success.
    def record_fingerprint(input_digests, declared)
      return false if input_digests.nil?

      output = Rake::FingerprintManifest.digest_file(name)
      return false if output.nil?

      entry = {
        "scope" => declared.sort,
        "inputs" => input_digests,
        "parameters" => current_fingerprint_parameters_digest,
        "output" => { "digest" => output, "size" => File.size(name) }
      }
      fingerprint_manifest.record(name, entry)
      @fingerprint_status = { reason: nil }
      true
    end

    def fingerprint_trace(status)
      return unless application.respond_to?(:trace)
      return unless application_options && application_options.trace

      detail = status[:detail]
      detail = " #{detail.inspect}" if detail
      application.trace "** Fingerprint #{status[:reason]} for #{name}#{detail}"
    end

    # ----------------------------------------------------------------
    # Task class methods.
    #
    class << self
      # Default fingerprint mode for file tasks.  Timestamp mode is used
      # unless this is enabled (for example by the --fingerprint option).
      def fingerprint_mode
        defined?(@fingerprint_mode) ? @fingerprint_mode : false
      end
      attr_writer :fingerprint_mode

      # Default manifest path used when neither the task nor the application
      # specifies one.
      def fingerprint_manifest_path
        @fingerprint_manifest_path ||= Rake::FingerprintManifest::DEFAULT_PATH
      end
      attr_writer :fingerprint_manifest_path

      # Apply the scope to the task name according to the rules for this kind
      # of task.  File based tasks ignore the scope when creating the name.
      def scope_name(scope, task_name)
        Rake.from_pathname(task_name)
      end
    end
  end
end
