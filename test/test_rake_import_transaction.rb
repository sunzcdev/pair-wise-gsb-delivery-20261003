# frozen_string_literal: true
require_relative "helper"

class TestRakeImportTransaction < Rake::TestCase
  def setup
    super
    @app = Rake.application
  end

  def test_successful_import_publishes_tasks_rules_and_dependencies
    File.write "good.rake", <<~RAKE
      task "base" => "extra_dep"
      task "from_import"
      rule ".xyz" => ".abc"
    RAKE
    Rake::Task.define_task("base")

    @app.add_import("good.rake")
    @app.load_imports

    assert Rake::Task.task_defined?("from_import")
    assert_equal ["extra_dep"], Rake::Task["base"].prerequisites

    FileUtils.touch "test.abc"
    assert Rake::Task["test.xyz"] # synthesized by the imported rule
  end

  def test_failed_import_rolls_back_partial_definitions
    Rake::Task.define_task("base")
    File.write "bad.rake", <<~RAKE
      task "base" => "extra_dep"
      task("base") { }
      task "half_defined"
      file "generated.txt"
      namespace "ns" do
        task "inner"
      end
      rule ".xyz" => ".abc"
      raise "boom"
    RAKE

    ex = assert_raises(RuntimeError) { @app.add_import("bad.rake"); @app.load_imports }
    assert_equal "boom", ex.message

    # New tasks and rules defined before the error are gone.
    refute Rake::Task.task_defined?("half_defined")
    refute Rake::Task.task_defined?("generated.txt")
    refute Rake::Task.task_defined?("ns:inner")
    FileUtils.touch "test.abc"
    assert_raises(RuntimeError) { Rake::Task["test.xyz"] }

    # Enhancements to pre-existing tasks are reverted too.
    assert_equal [], Rake::Task["base"].prerequisites
    assert_equal [], Rake::Task["base"].actions
  end

  def test_failed_import_restores_pending_description
    desc "kept description"
    File.write "bad.rake", <<~RAKE
      desc "imported description"
      task "t1"
      raise "boom"
    RAKE

    assert_raises(RuntimeError) { @app.add_import("bad.rake"); @app.load_imports }

    assert_equal "kept description", @app.last_description
  end

  def test_failed_import_is_not_recorded_and_can_be_retried
    File.write "flaky.rake", <<~RAKE
      task "half_defined"
      raise "boom"
    RAKE

    assert_raises(RuntimeError) { @app.add_import("flaky.rake"); @app.load_imports }
    refute Rake::Task.task_defined?("half_defined")

    # Fix the file, then retry: the failed import was re-queued.
    File.write "flaky.rake", <<~RAKE
      task "half_defined"
      task("after_fix") { }
    RAKE
    @app.load_imports

    assert Rake::Task.task_defined?("half_defined")
    assert Rake::Task.task_defined?("after_fix")
  end

  def test_path_aliases_of_same_file_are_imported_once
    File.write "aliases.rake", <<~RAKE
      $aliases_load_count = ($aliases_load_count || 0) + 1
      task "aliased"
    RAKE

    @app.add_import("aliases.rake")
    @app.add_import("./aliases.rake")
    @app.add_import(File.expand_path("aliases.rake"))
    @app.load_imports

    assert_equal 1, $aliases_load_count
    assert Rake::Task.task_defined?("aliased")
  ensure
    $aliases_load_count = nil
  end

  def test_remaining_imports_still_load_after_an_alias_is_skipped
    File.write "one.rake", 'task "one"'
    File.write "two.rake", 'task "two"'

    @app.add_import("one.rake")
    @app.add_import("./one.rake")
    @app.add_import("two.rake")
    @app.load_imports

    assert Rake::Task.task_defined?("one")
    assert Rake::Task.task_defined?("two")
  end

  def test_clearing_tasks_during_import_is_rejected
    Rake::Task.define_task("base")
    File.write "bad.rake", <<~RAKE
      task "doomed"
      Rake::Task["doomed"].clear
    RAKE

    ex = assert_raises(Rake::UnsupportedImportOperation) do
      @app.add_import("bad.rake")
      @app.load_imports
    end
    assert_match(/clear/, ex.message)

    # The rejection is itself rolled back along with the import.
    refute Rake::Task.task_defined?("doomed")
    assert Rake::Task.task_defined?("base")
  end

  def test_clearing_all_tasks_during_import_is_rejected
    Rake::Task.define_task("base")
    File.write "bad.rake", "Rake::Task.clear\n"

    assert_raises(Rake::UnsupportedImportOperation) do
      @app.add_import("bad.rake")
      @app.load_imports
    end

    assert Rake::Task.task_defined?("base")
  end

  def test_clearing_prerequisites_during_import_is_rejected
    Rake::Task.define_task("base" => ["old_dep"])
    File.write "bad.rake", <<~RAKE
      Rake::Task["base"].clear_prerequisites
    RAKE

    assert_raises(Rake::UnsupportedImportOperation) do
      @app.add_import("bad.rake")
      @app.load_imports
    end

    assert_equal ["old_dep"], Rake::Task["base"].prerequisites
  end

  def test_circular_import_is_rejected_before_loading
    loader = Object.new
    def loader.load(fn)
      Rake.application.add_import(fn)
      Rake.application.load_imports
    end
    @app.add_loader("dummy", loader)

    ex = assert_raises(RuntimeError) do
      @app.add_import("x.dummy")
      @app.load_imports
    end
    assert_match(/circular import/i, ex.message)
  end

  def test_import_transaction_allows_nesting
    File.write "inner.rake", 'task "inner_task"'
    loader = Object.new
    def loader.load(fn)
      Rake.application.load_imports
    end
    @app.add_loader("dummy", loader)

    @app.add_import("outer.dummy")
    @app.add_import("inner.rake")
    @app.load_imports

    assert Rake::Task.task_defined?("inner_task")
  end

  def test_external_side_effects_are_not_rolled_back
    File.write "bad.rake", <<~RAKE
      File.write("side_effect.txt", "ran")
      task "half_defined"
      raise "boom"
    RAKE

    assert_raises(RuntimeError) { @app.add_import("bad.rake"); @app.load_imports }

    # Declarative definitions are rolled back ...
    refute Rake::Task.task_defined?("half_defined")
    # ... but arbitrary Ruby side effects are not (and are not
    # promised to be).
    assert File.exist?("side_effect.txt")
  end
end