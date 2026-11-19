# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "fileutils"

# Unit tests for the controlled artifact protocol itself, independent of any
# task lifecycle.  See doc/artifacts.rdoc for the contract under test.
class TestRakeArtifact < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super
    Task.clear
  end

  def artifacts(*outputs, **options)
    Rake::ArtifactSet.new(outputs, **options)
  end

  # -- staging -------------------------------------------------------------

  def test_stage_path_is_dotted_sibling_of_output
    set = artifacts("build/out.txt")

    staging = set.stage_path

    assert_equal "build", File.dirname(staging)
    assert_match(%r{\A\.out\.txt\.rake-artifact-\d+-\d+\z}, File.basename(staging))
    assert File.directory?("build"), "stage_path creates the parent directory"
    refute File.exist?("build/out.txt"), "stage_path must not create the output"
  end

  def test_stage_path_is_stable_per_output
    set = artifacts("a.txt", "b.txt")

    assert_equal set.stage_path("a.txt"), set.stage_path("a.txt")
    refute_equal set.stage_path("a.txt"), set.stage_path("b.txt")
    refute_equal set.stage_path("a.txt"), set.stage_path("b.txt"),
                 "staging names must differ between outputs"
  end

  def test_stage_path_requires_a_name_for_multi_output_sets
    set = artifacts("a.txt", "b.txt")

    error = assert_raise(ArgumentError) { set.stage_path }
    assert_match(/output name is required/, error.message)
    assert_match(/\.a\.txt\.rake-artifact-/, artifacts("a.txt").stage_path)
  end

  def test_staging_names_do_not_collide_between_sets
    first = artifacts("out.txt").stage_path
    second = artifacts("out.txt").stage_path

    refute_equal first, second
  end

  def test_staging_pattern_matches_only_own_output
    set = artifacts("out.txt")
    set.write { |io| io.write("x") }
    File.write(".other.txt.rake-artifact-1-1", "fake")

    assert_equal [set.stage_path.sub(%r{\A\./}, "")],
                 Rake::ArtifactSet.staging_files(".", basename: "out.txt")
  end

  def test_write_closes_the_staging_io_before_returning
    set = artifacts("out.txt")
    captured = nil

    returned = set.write do |io|
      captured = io
      io.write("payload")
    end

    assert_predicate captured, :closed?, "the staging IO must be closed before publish"
    assert_equal set.stage_path, returned
    assert_equal "payload", File.binread(set.stage_path)
    refute File.exist?("out.txt")
  end

  def test_each_yields_outputs_in_declaration_order
    set = artifacts("b.txt", "a.txt")

    assert_equal [["b.txt", set.stage_path("b.txt")],
                  ["a.txt", set.stage_path("a.txt")]], set.each.to_a
  end

  def test_stage_paths_reports_the_paths_used_so_far
    set = artifacts("a.txt", "b.txt")

    assert_empty set.stage_paths
    staging = set.stage_path("a.txt")

    assert_equal({ "a.txt" => staging }, set.stage_paths)
    assert set.staging_file?(staging)
    refute set.staging_file?("a.txt")
  end

  def test_validators_reports_the_declared_checks
    one = ->(_p) { true }
    two = ->(_p) { true }
    set = artifacts("a.txt", "b.txt", validate: { "a.txt" => one })
    set.validate("b.txt", two)

    assert_equal({ "a.txt" => one, "b.txt" => two }, set.validators)
  end

  # -- validation ----------------------------------------------------------

  def test_validate_accepts_truthy_result
    set = artifacts("out.txt", validate: ->(path) { File.size(path) == 3 })
    set.write { |io| io.write("abc") }

    set.publish!

    assert_equal "abc", File.read("out.txt")
    assert_equal ["out.txt"], set.published
    assert_predicate set, :published?
  end

  def test_validate_rejects_false_without_touching_the_output
    set = artifacts("out.txt", validate: ->(_path) { false })
    set.write { |io| io.write("partial") }

    error = assert_raise(ArtifactValidationError) { set.publish! }

    assert_equal "out.txt", error.output
    assert_empty error.published
    refute File.exist?("out.txt")
    assert File.exist?(set.stage_path), "a rejection keeps the staged file for inspection"
  end

  def test_validate_reports_the_raised_error_as_cause
    failure = Class.new(StandardError)
    set = artifacts("out.txt", validate: ->(_path) { raise failure, "broken header" })
    set.write { |io| io.write("x") }

    error = assert_raise(ArtifactValidationError) { set.publish! }

    assert_kind_of failure, error.cause
    assert_match(/broken header/, error.message)
    refute File.exist?("out.txt")
  end

  def test_missing_staged_file_is_a_validation_error
    set = artifacts("out.txt", validate: ->(_path) { true })

    error = assert_raise(ArtifactValidationError) { set.publish! }

    assert_match(/was not written to its staging file/, error.message)
    refute File.exist?("out.txt")
  end

  def test_validator_receives_staging_path_and_output_name
    set = artifacts("out.txt")
    seen = []
    set.validate do |name, path|
      seen << [name, path]
      true
    end
    set.write { |io| io.write("x") }

    set.publish!

    assert_equal 1, seen.size
    assert_equal "out.txt", seen.first[0]
    assert_match(%r{\A\.out\.txt\.rake-artifact-\d+-\d+\z}, File.basename(seen.first[1]))
  end

  def test_validator_runs_exactly_once_per_publish
    checks = 0
    set = artifacts("out.txt", validate: ->(_path) { checks += 1; true })
    set.write { |io| io.write("x") }

    set.publish!
    set.publish! # second call must not re-run anything

    assert_equal 1, checks
  end

  def test_validate_rejects_unknown_output
    set = artifacts("out.txt")

    assert_raise(ArgumentError) { set.validate("other.txt") { true } }
  end

  def test_open_refuses_to_restage_a_published_output
    set = artifacts("out.txt")
    set.write { |io| io.write("done") }
    set.publish!

    assert_raise(ArtifactError) do
      set.write { |io| io.write("again") }
    end
  end

  # -- per-output publication ---------------------------------------------

  def test_publish_is_per_output_and_keeps_earlier_outputs
    set = artifacts("a.txt", "b.txt",
                    validate: { "a.txt" => ->(_path) { true },
                                "b.txt" => ->(_path) { false } })
    set.write("a.txt") { |io| io.write("A") }
    set.write("b.txt") { |io| io.write("B") }

    error = assert_raise(ArtifactValidationError) { set.publish! }

    assert_equal ["a.txt"], error.published
    assert_equal "A", File.read("a.txt"), "the first output stays published"
    refute File.exist?("b.txt")
    refute_predicate set, :published?
  end

  def test_publish_accepts_an_explicit_subset
    set = artifacts("a.txt", "b.txt")
    set.write("a.txt") { |io| io.write("A") }
    set.write("b.txt") { |io| io.write("B") }

    set.publish!("b.txt")

    assert_equal ["b.txt"], set.published
    assert_equal "B", File.read("b.txt")
    refute File.exist?("a.txt")

    set.publish!("a.txt")

    assert_predicate set, :published?
  end

  def test_publish_does_not_renumber_or_reorder_outputs
    order = []
    set = artifacts("a.txt", "b.txt")
    set.validate("a.txt") { order << "a"; true }
    set.validate("b.txt") { order << "b"; true }
    set.write("b.txt") { |io| io.write("B") }
    set.write("a.txt") { |io| io.write("A") }

    set.publish!

    assert_equal %w[a b], order, "outputs publish in declaration order"
  end

  # -- cleanup ------------------------------------------------------------

  def test_cleanup_removes_only_unpublished_staging_files
    set = artifacts("a.txt", "b.txt")
    set.validate("b.txt") { false }
    set.write("a.txt") { |io| io.write("A") }
    set.write("b.txt") { |io| io.write("B") }
    assert_raise(ArtifactValidationError) { set.publish! }

    leftovers = set.cleanup!

    assert_empty leftovers
    refute File.exist?(set.stage_path("b.txt"))
    assert_equal "A", File.read("a.txt")
  end

  def test_cleanup_reports_files_it_could_not_remove
    set = artifacts("out.txt")
    blocked = set.stage_path
    Dir.mkdir(blocked)
    File.write(File.join(blocked, "child"), "x")

    leftovers = set.cleanup!

    assert_equal [blocked], leftovers
    Dir.rmdir(File.join(blocked, "child")) rescue nil
    File.delete(File.join(blocked, "child")) rescue nil
  end

  def test_remove_unpublished_new_outputs_deletes_only_new_files
    File.write("old.txt", "ORIGINAL")
    set = artifacts("old.txt", "new.txt")
    set.validate("old.txt") { false }
    File.write("new.txt", "written directly by the action")

    leftovers = set.remove_unpublished_new_outputs!

    assert_empty leftovers
    assert_equal "ORIGINAL", File.read("old.txt"), "a preexisting output is preserved"
    refute File.exist?("new.txt"), "a new output left behind by a failed build is removed"
  end

  def test_remove_unpublished_outputs_never_touches_published_files
    set = artifacts("out.txt")
    set.write { |io| io.write("done") }
    set.publish!

    assert_empty set.remove_unpublished_new_outputs!
    assert_equal "done", File.read("out.txt")
  end

  def test_sweep_removes_only_staging_files
    File.write(".out.txt.rake-artifact-1-1", "stale")
    File.write("out.txt", "real output")
    File.write(".unrelated", "keep me")

    leftovers = Rake::ArtifactSet.sweep!(".")

    assert_empty leftovers
    refute File.exist?(".out.txt.rake-artifact-1-1")
    assert_equal "real output", File.read("out.txt")
    assert File.exist?(".unrelated")
  end

  # -- declaration checks -------------------------------------------------

  def test_duplicate_outputs_are_rejected
    assert_raise(ArgumentError) { artifacts("a.txt", "a.txt") }
  end

  def test_empty_output_list_is_rejected
    assert_raise(ArgumentError) { artifacts }
  end

  def test_hash_validation_requires_callable_values
    assert_raise(ArgumentError) { artifacts("a.txt", validate: { "a.txt" => "nope" }) }
  end

  def test_hash_validation_rejects_unknown_outputs
    assert_raise(ArgumentError) { artifacts("a.txt", validate: { "b.txt" => ->(_) { true } }) }
  end

  def test_missing_staged_output_wins_over_a_missing_validator
    set = artifacts("a.txt")

    error = assert_raise(ArtifactValidationError) { set.publish! }

    assert_equal "a.txt", error.output
  end
end
