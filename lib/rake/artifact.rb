# frozen_string_literal: true
require "fileutils"
require "monitor"

module Rake

  # Base class for every error raised by the controlled artifact interface.
  # See Rake::ArtifactSet, Rake::FileTask#artifacts and doc/artifacts.rdoc.
  class ArtifactError < StandardError
  end

  # Raised when a staged artifact does not satisfy its declared checks, so it
  # is never published as the task output.
  #
  # +output+ names the rejected artifact and +published+ lists the outputs of
  # the same set that had already been published when the rejection happened.
  class ArtifactValidationError < ArtifactError
    # Output whose validation failed.
    attr_reader :output

    # Outputs published before the failure.
    attr_reader :published

    def initialize(message = nil, output: nil, published: [])
      super(message)
      @output = output
      @published = published || []
    end
  end

  # Raised when a validated artifact could not be moved onto its final name,
  # for example because the target name is an existing directory or a file
  # that is still open on a platform that forbids renaming over it.
  #
  # +output+ names the artifact that could not be published and +published+
  # lists the outputs published before the failure.
  class ArtifactPublishError < ArtifactError
    # Output that could not be published.
    attr_reader :output

    # Outputs published before the failure.
    attr_reader :published

    def initialize(message = nil, output: nil, published: [])
      super(message)
      @output = output
      @published = published || []
    end
  end

  # Raised when a failed artifact task could not remove every file it created.
  #
  # Cleanup failures are never silent: the offending paths are listed in
  # +leftovers+ and the build failure that triggered the cleanup is available
  # as +cause+ (also via #original_error).  Leftover staging files keep their
  # <tt>.<name>.rake-artifact-*</tt> names, so they can never be mistaken for
  # the task output by a later FileTask.
  class ArtifactCleanupError < ArtifactError
    # Paths that were created by the failed build and could not be removed.
    attr_reader :leftovers

    def initialize(message = nil, leftovers: [])
      super(message)
      @leftovers = leftovers || []
    end

    # The build, validation or publication failure whose cleanup left files
    # behind.
    def original_error
      cause
    end
  end

  # Set of output files produced by one task action under the controlled
  # artifact protocol.
  #
  # The action never writes a final output.  It writes to the per-output
  # staging path returned by #stage_path, declares checks with #validate and
  # then calls #publish!, which validates and renames each staged file onto
  # its final name.  Until that rename the previous target (if any) is left
  # untouched, so a failed build keeps the original output and a partially
  # written file never appears under the final name.
  #
  # Publishing is done output by output in declaration order.  Each output is
  # validated and renamed independently; if a later output is rejected the
  # outputs published before it stay in place.  This is a per-output
  # publication guarantee, not a cross-file transaction: see
  # doc/artifacts.rdoc for how to obtain all-or-nothing behaviour.
  class ArtifactSet
    # Marker embedded in every staging file name.
    STAGING_MARKER = "rake-artifact"

    class << self
      # Glob pattern matching the staging files of +basename+ in one
      # directory, for example <tt>".out.txt.rake-artifact-*"</tt>.
      def staging_pattern(basename)
        ".#{basename}.#{STAGING_MARKER}-*"
      end

      # Staging files currently present in +dir+, sorted.  Dot files are
      # included because staging files always start with a dot.  Pass
      # +basename+ to restrict the result to the staging files of one output.
      def staging_files(dir = ".", basename: "*")
        glob = File.join(dir, staging_pattern(basename))
        Dir.glob(glob).sort.map { |path| path.sub(%r{\A\./}, "") }
      end

      # Remove the staging files left in +dir+ by interrupted runs (a killed
      # process cannot run its own cleanup).  Only files matching the staging
      # pattern are touched, so a real output is never removed.
      #
      # Returns the paths that could not be removed, so a failure stays
      # visible to the caller.  Never raises.
      def sweep!(dir = ".")
        leftovers = []
        staging_files(dir).each do |path|
          begin
            File.delete(path)
          rescue Errno::ENOENT
            # Already gone.
          rescue SystemCallError, IOError
            # Reported below.
          end
          leftovers << path if File.exist?(path) || File.symlink?(path)
        end
        leftovers
      end

      # Normalize a task name or Pathname into the string Rake uses for file
      # task names.
      def normalize_path(path) # :nodoc:
        path = path.to_path if path.respond_to?(:to_path)
        path = path.to_str if path.respond_to?(:to_str)
        path.to_s
      end

      # Serial number that makes staging names unique inside one process.
      def next_serial # :nodoc:
        lock.synchronize { @serial = (@serial || 0) + 1 }
      end

      def lock # :nodoc:
        @lock ||= Monitor.new
      end
      private :lock
    end

    # Declared output names in publication order.  The first entry is the
    # task's own name when the set belongs to a FileTask.
    attr_reader :outputs

    # Owning FileTask, or nil for a standalone set.
    attr_reader :task

    # Outputs that have been published so far, in publication order.
    attr_reader :published

    def initialize(outputs, task: nil, validate: nil)
      @outputs = Array(outputs).map { |output| self.class.normalize_path(output) }
      raise ArgumentError, "an artifact set needs at least one output" if @outputs.empty?
      raise ArgumentError, "duplicate artifact output in #{@outputs.inspect}" if
        @outputs.uniq.size != @outputs.size

      @task = task
      @validators = {}
      @published = []
      @stage_paths = {}
      @preexisting = {}
      @token = self.class.next_serial
      @outputs.each { |output| @preexisting[output] = File.exist?(output) || File.symlink?(output) }
      add_declared_validators(validate)
    end

    # Staging path for +output+.  With a single declared output the name may
    # be omitted.  The parent directory is created so the action can write the
    # file; the final output is not touched here.
    def stage_path(output = nil)
      output = resolve_output(output)
      @stage_paths[output] ||= begin
        dir = File.dirname(output)
        FileUtils.mkdir_p(dir) unless File.directory?(dir)
        File.join(dir, ".#{File.basename(output)}.#{STAGING_MARKER}-#{Process.pid}-#{@token}")
      end
    end
    alias path stage_path
    alias [] stage_path

    # Copy of the output name to staging path mapping used so far.
    def stage_paths
      @stage_paths.dup
    end

    # Copy of the declared output name to validator mapping.
    def validators
      @validators.dup
    end

    # Clear the staged and published bookkeeping so the set can describe a new
    # build attempt.  The declaration, the validators and the object identity
    # are kept, so a Rakefile that captured the set at declaration time keeps
    # staging its outputs through the same object on every invocation.
    #
    # Returns self.
    def reset!
      @stage_paths = {}
      @published = []
      @outputs.each { |output| @preexisting[output] = File.exist?(output) || File.symlink?(output) }
      self
    end

    # Yield every declared output together with its staging path.
    def each
      return enum_for(:each) unless block_given?
      @outputs.each { |output| yield output, stage_path(output) }
    end

    # Open the staging file for +output+ and yield the IO.  The file is closed
    # before this method returns, so a later rename cannot be blocked by an
    # open handle of our own.  Returns the staging path.
    #
    # An output that has already been published cannot be staged again: that
    # would leave a writable file next to a finished artifact.
    def open(output = nil, mode = "wb")
      raise ArgumentError, "artifact open requires a block" unless block_given?
      output = resolve_output(output)
      if @published.include?(output)
        raise ArtifactError, "artifact #{output.inspect} is already published"
      end
      staging = stage_path(output)
      File.open(staging, mode) { |io| yield io }
      staging
    end

    # Open the staging file in binary write mode.  Shorthand for #open.
    def write(output = nil, &block)
      open(output, "wb", &block)
    end

    # Declare a check for +output+, or for every declared output when no name
    # is given.
    #
    # The check runs against the already written staging file.  It is a
    # validation, not a build: it may inspect (or even read back) the staged
    # file but must not invoke the task again, run build actions or write the
    # final output.  Rake runs each check exactly once per publication attempt
    # and never re-runs the action to "confirm" a check.
    #
    # The block may take zero arguments, the staging path, or the output name
    # and the staging path.  It accepts the artifact by returning a truthy
    # value; returning false or raising rejects it.
    #
    #   artifacts.validate("out.txt") { |path| File.size(path) > 0 }
    #   artifacts.validate { |name, path| check(name, path) }
    def validate(output = nil, callable = nil, &block)
      block ||= callable
      raise ArgumentError, "artifact validate requires a callable" unless block
      if output.nil?
        @outputs.each { |name| @validators[name] = block }
      else
        add_validator(resolve_output(output), block)
      end
      self
    end

    # Declare several checks at once.  +spec+ is either a single callable
    # applied to every output or a Hash mapping an output name to its
    # callable.  Nil declares nothing.  Returns self.
    def declare_validations(spec)
      add_declared_validators(spec)
      self
    end

    # Validate and publish the given outputs, defaulting to all of them and
    # always in declaration order.  Already published outputs are skipped, so
    # the method is idempotent and can be called once per output:
    #
    #   artifacts.publish!("report.json")
    #   artifacts.publish!("index.html")
    #
    # Each output is validated first and renamed immediately after its own
    # check succeeds.  Outputs published earlier are never rolled back when a
    # later output fails; exceptions carry the +published+ list so the partial
    # result is explicit.  Returns self.
    def publish!(*wanted)
      names = wanted.empty? ? @outputs : wanted.map { |name| resolve_output(name) }
      unknown = names - @outputs
      unless unknown.empty?
        raise ArgumentError, "unknown artifact output #{unknown.inspect}"
      end
      (@outputs & names).each do |output|
        next if @published.include?(output)
        staging = stage_path(output)
        run_validators_for(output, staging)
        rename_staged_output(output, staging)
        @published << output
      end
      self
    end
    alias publish publish!

    # True when every declared output has been published.
    def published?
      @published.size == @outputs.size
    end

    # Remove staging files that were not published.  Returns the paths that
    # could not be removed; an empty array means every staging file is gone.
    #
    # Cleanup never raises: the caller decides how to report leftovers, so a
    # failure to remove a staging file stays visible in the result.
    def cleanup!
      leftovers = []
      @stage_paths.each do |output, staging|
        next if @published.include?(output)
        next unless File.exist?(staging) || File.symlink?(staging)
        begin
          File.delete(staging)
        rescue Errno::ENOENT
          # Already gone; nothing to clean.
        rescue SystemCallError, IOError
          # Reported below through the leftover check.
        end
        leftovers << staging if File.exist?(staging) || File.symlink?(staging)
      end
      leftovers.uniq
    end

    # Remove final outputs that did not exist when this set was created and
    # that are still unpublished.  Such a file can only be an incomplete
    # leftover of the failed action (the protocol never publishes without
    # validating), and leaving it behind would make a later FileTask treat it
    # as a finished output.  Outputs that existed before the build are never
    # touched.
    #
    # Returns the paths that could not be removed.  Never raises.
    def remove_unpublished_new_outputs!
      leftovers = []
      @outputs.each do |output|
        next if @published.include?(output)
        next if @preexisting[output]
        next unless File.exist?(output) || File.symlink?(output)
        begin
          File.delete(output)
        rescue Errno::ENOENT
          # Already gone.
        rescue SystemCallError, IOError
          # Reported below.
        end
        leftovers << output if File.exist?(output) || File.symlink?(output)
      end
      leftovers
    end

    # True when +path+ looks like a staging file of this set.
    def staging_file?(path)
      @stage_paths.value?(path)
    end

    def inspect # :nodoc:
      "<#{self.class} #{@outputs.inspect} published=#{@published.inspect}>"
    end

    private

    def add_declared_validators(declared)
      case declared
      when nil
        nil
      when Hash
        declared.each do |output, callable|
          unless callable.respond_to?(:call)
            raise ArgumentError, "validator for #{output.inspect} is not callable"
          end
          add_validator(resolve_output(output), callable)
        end
      else
        unless declared.respond_to?(:call)
          raise ArgumentError, "validate: must be callable or a Hash of output => callable"
        end
        @outputs.each { |output| @validators[output] = declared }
      end
    end

    def add_validator(output, callable)
      unless @outputs.include?(output)
        raise ArgumentError, "unknown artifact output #{output.inspect}"
      end
      @validators[output] = callable
      self
    end

    def resolve_output(output)
      return self.class.normalize_path(output) unless output.nil?
      return @outputs.first if @outputs.size == 1
      raise ArgumentError,
            "output name is required for an artifact set with #{@outputs.size} outputs"
    end

    def run_validators_for(output, staging)
      unless File.file?(staging)
        raise ArtifactValidationError.new(
          "artifact #{output.inspect} was not written to its staging file #{staging.inspect}",
          output: output,
          published: @published.dup
        )
      end

      validator = @validators[output]
      return if validator.nil?

      accepted =
        begin
          call_validator(validator, output, staging)
        rescue Exception => ex
          raise ArtifactValidationError.new(
            "artifact #{output.inspect} failed validation: #{ex.message}",
            output: output,
            published: @published.dup
          ), cause: ex
        end

      return if accepted

      raise ArtifactValidationError.new(
        "artifact #{output.inspect} failed validation",
        output: output,
        published: @published.dup
      )
    end

    def call_validator(validator, output, staging)
      case validator.arity
      when 0
        validator.call
      when 1
        validator.call(staging)
      else
        validator.call(output, staging)
      end
    end

    def rename_staged_output(output, staging)
      File.rename(staging, output)
    rescue SystemCallError, IOError => ex
      raise ArtifactPublishError.new(
        "could not publish artifact #{output.inspect}: #{ex.message}",
        output: output,
        published: @published.dup
      ), cause: ex
    end
  end
end
