# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "fileutils"

# Behaviour of Rake::FileTask with declared controlled artifacts.  These
# tests pin the lifecycle contract: staging, validation, publication, the
# failed-build cleanup and the guarantee that a leftover file never makes the
# next invocation look finished.
class TestRakeFileTaskArtifacts < Rake::TestCase # :nodoc:
  include Rake

  OUT = "out.txt"
  SIG = "out.txt.sig"
  SRC = "src.in"

  def setup
    super
    Task.clear
    @runs = 0
  end

  # A task whose action stages one output through the artifact set.
  def build_task(*outputs, validate: nil)
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(*outputs, validate: validate)
    task.enhance do |t|
      @runs += 1
      write_declared_outputs(t.artifact_set)
    end
    task
  end

  def write_declared_outputs(set)
    set.outputs.each do |output|
      set.write(output) { |io| io.write("payload:#{output}") }
    end
  end

  # Make the task out of date even though its output file exists, by making a
  # prerequisite newer.  A file task is only rebuilt when it is needed.
  def force_rebuild(task)
    File.write(SRC, "source")
    File.utime(Time.now + 5, Time.now + 5, SRC)
    task.enhance([SRC])
    task
  end

  # -- happy path ---------------------------------------------------------

  def test_publishes_the_staged_output
    task = build_task

    task.invoke

    assert_equal "payload:#{OUT}", File.read(OUT)
    assert_equal 1, @runs
    assert_empty Rake::ArtifactSet.staging_files(".", basename: OUT)
  end

  def test_declaration_inside_action_also_publishes
    task = Rake::FileTask.define_task(OUT)
    task.enhance do |t|
      @runs += 1
      art = t.artifacts(validate: ->(path) { File.size(path) > 0 })
      art.write { |io| io.write("inline") }
    end

    task.invoke

    assert_equal "inline", File.read(OUT)
    assert_equal 1, @runs
  end

  def test_published_output_is_not_rebuilt
    task = build_task
    task.invoke

    task.reenable
    task.invoke

    assert_equal 1, @runs, "a published output must satisfy the task"
  end

  def test_artifacts_returns_the_same_set_for_repeated_calls
    task = Rake::FileTask.define_task(OUT)

    first = task.artifacts
    second = task.artifacts

    assert_same first, second
  end

  def test_outputs_must_include_the_task_name
    task = Rake::FileTask.define_task(OUT)

    error = assert_raise(ArgumentError) { task.artifacts("other.txt") }
    assert_match(/must include the task name/, error.message)
  end

  def test_pathname_outputs_are_normalized
    task = Rake::FileTask.define_task(OUT)

    set = task.artifacts(Pathname.new(OUT))

    assert_equal [OUT], set.outputs
  end

  # -- failure: original output preserved --------------------------------

  def test_failed_action_keeps_the_previous_output_and_its_mtime
    File.write(OUT, "ORIGINAL")
    stamp = Time.now - 3600
    File.utime(stamp, stamp, OUT)

    task = force_rebuild(build_task)
    task.clear_actions
    task.enhance do |t|
      @runs += 1
      t.artifact_set.write { |io| io.write("PARTIAL") }
      raise "build exploded"
    end

    error = assert_raise(RuntimeError) { task.invoke }

    assert_match(/build exploded/, error.message)
    assert_equal "ORIGINAL", File.read(OUT)
    assert_equal stamp.to_i, File.mtime(OUT).to_i, "the original timestamp is untouched"
    assert_empty Rake::ArtifactSet.staging_files(".", basename: OUT)
  end

  def test_failed_action_never_leaves_a_new_output_behind
    task = Rake::FileTask.define_task(OUT)
    task.artifacts
    task.enhance do |t|
      t.artifact_set.write { |io| io.write("PARTIAL") }
      raise "nope"
    end

    assert_raise(RuntimeError) { task.invoke }

    refute File.exist?(OUT), "no output file may appear for a failed build"
    assert_empty Rake::ArtifactSet.staging_files(".", basename: OUT)
  end

  def test_incomplete_output_written_directly_is_not_mistaken_for_success
    task = Rake::FileTask.define_task(OUT)
    task.artifacts
    task.enhance do |_t|
      File.write(OUT, "only half")
      raise "aborted after opening the target"
    end

    assert_raise(RuntimeError) { task.invoke }
    refute File.exist?(OUT)

    task.reenable
    assert_predicate task, :needed?, "the next invocation must rebuild"
  end

  def test_failed_rebuild_does_not_shadow_a_good_previous_build
    task = build_task
    task.invoke
    assert_equal "payload:#{OUT}", File.read(OUT)

    force_rebuild(task)
    task.clear_actions
    task.enhance do |t|
      @runs += 1
      t.artifact_set.write { |io| io.write("BROKEN") }
      raise "late failure"
    end
    task.reenable

    assert_raise(RuntimeError) { task.invoke }

    assert_equal "payload:#{OUT}", File.read(OUT)
  end

  # -- validation ---------------------------------------------------------

  def test_validation_failure_keeps_the_previous_output
    File.write(OUT, "ORIGINAL")
    task = force_rebuild(build_task(validate: ->(_path) { false }))

    error = assert_raise(Rake::ArtifactValidationError) { task.invoke }

    assert_equal OUT, error.output
    assert_equal "ORIGINAL", File.read(OUT)
    assert_empty Rake::ArtifactSet.staging_files(".", basename: OUT)
  end

  def test_validation_failure_cleans_up_a_new_output
    task = build_task(validate: ->(_path) { false })

    assert_raise(Rake::ArtifactValidationError) { task.invoke }

    refute File.exist?(OUT)
  end

  def test_validator_raising_keeps_the_original_error_as_cause
    boom = Class.new(StandardError)
    task = build_task(validate: ->(_path) { raise boom, "corrupt" })

    error = assert_raise(Rake::ArtifactValidationError) { task.invoke }

    assert_kind_of boom, error.cause
  end

  def test_validator_must_not_run_the_task_again
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(validate: ->(_path) { Rake::Task[OUT].reenable; Rake::Task[OUT].invoke })
    task.enhance { |t| @runs += 1; t.artifact_set.write { |io| io.write("x") } }

    error = assert_raise(Rake::ArtifactValidationError) { task.invoke }

    assert_match(/must not run the task again/, error.message)
    assert_equal 1, @runs, "a validator never rebuilds the artifact"
    refute File.exist?(OUT)
  end

  def test_action_and_validator_run_exactly_once
    checks = 0
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(validate: ->(_path) { checks += 1; true })
    task.enhance { |t| @runs += 1; t.artifact_set.write { |io| io.write("x") } }

    task.invoke

    assert_equal 1, @runs
    assert_equal 1, checks
  end

  # -- multiple outputs ---------------------------------------------------

  def test_each_output_is_validated_and_published_independently
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(OUT, SIG, validate: { OUT => ->(_p) { true }, SIG => ->(_p) { false } })
    task.enhance do |t|
      @runs += 1
      t.artifact_set.write(OUT) { |io| io.write("body") }
      t.artifact_set.write(SIG) { |io| io.write("sig") }
    end

    error = assert_raise(Rake::ArtifactValidationError) { task.invoke }

    assert_equal [OUT], error.published
    assert_equal "body", File.read(OUT), "an already published output stays in place"
    refute File.exist?(SIG)
    assert_empty Rake::ArtifactSet.staging_files(".", basename: SIG)
  end

  def test_multi_output_task_is_needed_when_one_output_disappears
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(OUT, SIG)
    task.enhance do |t|
      @runs += 1
      t.artifact_set.write(OUT) { |io| io.write("body") }
      t.artifact_set.write(SIG) { |io| io.write("sig") }
    end
    task.invoke
    assert_equal 1, @runs

    File.delete(SIG)
    task.reenable

    assert_predicate task, :needed?
    task.invoke
    assert_equal 2, @runs
    assert File.exist?(SIG)
  end

  def test_directory_at_output_name_does_not_satisfy_the_task
    task = Rake::FileTask.define_task(OUT)
    task.artifacts
    Dir.mkdir(OUT)

    assert_predicate task, :needed?

    Dir.rmdir(OUT)
  end

  def test_staging_files_do_not_affect_the_up_to_date_check
    File.write(OUT, "GOOD")
    File.write(".out.txt.rake-artifact-1-1", "leftover")
    task = Rake::FileTask.define_task(OUT)
    task.artifacts

    refute_predicate task, :needed?
  end

  # -- cleanup failures stay visible -------------------------------------

  def test_cleanup_failure_is_reported_with_leftovers_and_cause
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(validate: ->(_p) { false })
    task.enhance do |t|
      staging = t.artifact_set.stage_path
      Dir.mkdir(staging)
      File.write(File.join(staging, "child"), "busy")
    end

    error = assert_raise(Rake::ArtifactCleanupError) { task.invoke }

    refute_empty error.leftovers
    assert_match(/could not remove/, error.message)
    assert_not_nil error.cause, "the original failure stays reachable"
    assert_kind_of Rake::ArtifactValidationError, error.original_error
    refute File.exist?(OUT)
  end

  # -- dry run ------------------------------------------------------------

  def test_dry_run_does_not_touch_the_artifact_lifecycle
    task = Rake::FileTask.define_task(OUT)
    task.artifacts(validate: ->(_p) { true })
    task.enhance { |t| @runs += 1; t.artifact_set.write { |io| io.write("x") } }

    Rake.application.options.dryrun = true
    task.invoke

    assert_equal 0, @runs
    refute File.exist?(OUT)
  end

  # -- timestamp integrity ------------------------------------------------

  def test_publishing_uses_the_real_write_time
    task = build_task

    task.invoke

    assert_in_delta Time.now.to_f, File.mtime(OUT).to_f, 60,
                    "the published output keeps a real timestamp"
  end

  def test_cleanup_never_rewrites_the_preserved_output_timestamp
    File.write(OUT, "ORIGINAL")
    stamp = Time.now - 7200
    File.utime(stamp, stamp, OUT)
    task = force_rebuild(build_task(validate: ->(_p) { false }))

    assert_raise(Rake::ArtifactValidationError) { task.invoke }

    assert_equal stamp.to_i, File.mtime(OUT).to_i
  end
end
