# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)

# End-to-end checks driving a real rake subprocess.  They prove that the
# controlled artifact boundary survives between invocations: a failed build
# leaves no output that a later FileTask would accept.
class TestRakeArtifactFunctional < Rake::TestCase # :nodoc:
  include RubyRunner

  def rakefile_controlled_artifact
    rakefile <<~RAKEFILE
      file "out.txt" => ["in.txt"] do |t|
        art = t.artifacts(validate: { "out.txt" => ->(path) {
          File.read(path).end_with?("DONE\\n")
        } })
        art.write("out.txt") { |io| io.write(File.read("in.txt")) }
      end
    RAKEFILE
  end

  def test_successful_build_publishes_and_is_reused
    rakefile_controlled_artifact
    File.write("in.txt", "HELLO DONE\n")

    rake "out.txt"

    assert_equal "HELLO DONE\n", File.read("out.txt")
    assert_empty Dir.glob(".out.txt.rake-artifact-*")

    rake "out.txt"
    assert_predicate @exit, :success?
  end

  def test_failed_validation_leaves_no_output_across_processes
    rakefile <<~RAKEFILE
      file "out.txt" do |t|
        art = t.artifacts(validate: { "out.txt" => ->(path) {
          File.read(path).end_with?("DONE\\n")
        } })
        art.write("out.txt") { |io| io.write("TRUNCATED") }
      end
    RAKEFILE

    rake "out.txt"

    refute_predicate @exit, :success?
    refute File.exist?("out.txt"), "a rejected artifact must never appear as the output"
    assert_empty Dir.glob(".out.txt.rake-artifact-*")
  end

  def test_failed_action_preserves_the_previous_output_across_processes
    rakefile_controlled_artifact
    File.write("in.txt", "GOOD DONE\n")
    rake "out.txt"
    assert_equal "GOOD DONE\n", File.read("out.txt")
    stamp = File.mtime("out.txt")

    File.write("in.txt", "NEW DONE\n")
    File.utime(stamp - 60, stamp - 60, "in.txt")
    # Force a rebuild while making the action fail.
    rakefile <<~RAKEFILE
      file "out.txt" => ["in.txt"] do |t|
        art = t.artifacts
        art.write("out.txt") { |io| io.write("HALF") }
        raise "partial build"
      end
    RAKEFILE
    run_ruby(@ruby_options + [@rake_exec, "out.txt", "--always-make"])

    refute_predicate @exit, :success?
    assert_equal "GOOD DONE\n", File.read("out.txt")
    assert_equal stamp.to_i, File.mtime("out.txt").to_i
  end

  def test_leftover_staging_file_is_not_treated_as_the_output
    rakefile_controlled_artifact
    File.write("in.txt", "HELLO DONE\n")
    File.write(".out.txt.rake-artifact-1-1", "stale partial")

    rake "out.txt"

    assert_equal "HELLO DONE\n", File.read("out.txt")
    refute_equal "stale partial", File.read("out.txt")
  end
end
