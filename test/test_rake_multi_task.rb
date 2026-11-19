# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "thread"

class TestRakeMultiTask < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super

    Task.clear
    @runs = Array.new
    @mutex = Mutex.new
  end

  def teardown
    Rake.application.thread_pool.join

    super
  end

  def add_run(obj)
    @mutex.synchronize do
      @runs << obj
    end
  end

  def test_running_multitasks
    task :a do 3.times do |i| add_run("A#{i}"); sleep 0.01; end end
    task :b do 3.times do |i| add_run("B#{i}"); sleep 0.01;  end end
    multitask both: [:a, :b]
    Task[:both].invoke
    assert_equal 6, @runs.size
    assert @runs.index("A0") < @runs.index("A1")
    assert @runs.index("A1") < @runs.index("A2")
    assert @runs.index("B0") < @runs.index("B1")
    assert @runs.index("B1") < @runs.index("B2")
  end

  def test_all_multitasks_wait_on_slow_prerequisites
    task :slow do 3.times do |i| add_run("S#{i}"); sleep 0.05 end end
    task a: [:slow] do 3.times do |i| add_run("A#{i}"); sleep 0.01 end end
    task b: [:slow] do 3.times do |i| add_run("B#{i}"); sleep 0.01 end end
    multitask both: [:a, :b]
    Task[:both].invoke
    assert_equal 9, @runs.size
    assert @runs.index("S0") < @runs.index("S1")
    assert @runs.index("S1") < @runs.index("S2")
    assert @runs.index("S2") < @runs.index("A0")
    assert @runs.index("S2") < @runs.index("B0")
    assert @runs.index("A0") < @runs.index("A1")
    assert @runs.index("A1") < @runs.index("A2")
    assert @runs.index("B0") < @runs.index("B1")
    assert @runs.index("B1") < @runs.index("B2")
  end

  def test_multitasks_with_parameters
    task :a, [:arg] do |t, args| add_run(args[:arg]) end
    multitask :b, [:arg] => [:a] do |t, args| add_run(args[:arg] + "mt") end
    Task[:b].invoke "b"
    assert @runs[0] == "b"
    assert @runs[1] == "bmt"
  end

  def test_cross_thread_prerequisite_failures
    failed = false

    multitask :fail_once do
      fail_now = !failed
      failed = true
      raise "failing once" if fail_now
    end

    task a: :fail_once
    task b: :fail_once

    assert_raises RuntimeError do
      Rake::Task[:a].invoke
    end

    assert_raises RuntimeError do
      Rake::Task[:b].invoke
    end
  end

  def test_task_not_executed_if_dependant_task_failed_concurrently
    multitask default: [:one, :two]

    task :one do
      raise
    end

    task_two_was_executed = false
    task two: :one do
      task_two_was_executed = true
    end

    begin
      Rake::Task[:default].invoke
    rescue RuntimeError
    ensure
      sleep 0.5
      refute task_two_was_executed
    end
  end

  def test_parallel_sh_chdir_writes_each_output_in_its_own_directory
    omit "JRuby does not support spawn options" if jruby90?

    FileUtils.mkdir_p "alpha dir"
    FileUtils.mkdir_p "beta dir"
    original = Dir.pwd

    task :alpha do
      3.times do
        sh RUBY, "-e", "File.write('alpha.txt', 'a')",
          chdir: "alpha dir", verbose: false
      end
    end

    task :beta do
      3.times do
        sh RUBY, "-e", "File.write('beta.txt', 'b')",
          chdir: "beta dir", verbose: false
      end
    end

    multitask parallel_chdir: [:alpha, :beta]

    Rake::Task[:parallel_chdir].invoke

    assert File.exist?(File.join("alpha dir", "alpha.txt")),
      "alpha output must land in its own working directory"
    assert File.exist?(File.join("beta dir", "beta.txt")),
      "beta output must land in its own working directory"
    refute File.exist?("alpha.txt"), "no output may leak into the process directory"
    refute File.exist?("beta.txt"), "no output may leak into the process directory"
    assert_equal original, Dir.pwd
  end

  def test_parallel_in_dir_blocks_are_serialized_and_restore_pwd
    FileUtils.mkdir_p "one dir"
    FileUtils.mkdir_p "two dir"
    original = Dir.pwd
    inside_mutex = Mutex.new
    inside = {}
    concurrent = 0
    max_concurrent = 0

    task :one do
      in_dir "one dir" do
        inside_mutex.synchronize do
          inside[:one] = Dir.pwd
          concurrent += 1
          max_concurrent = concurrent if concurrent > max_concurrent
        end
        sleep 0.05
        File.write "one.txt", "1"
        inside_mutex.synchronize { concurrent -= 1 }
      end
    end

    task :two do
      in_dir "two dir" do
        inside_mutex.synchronize do
          inside[:two] = Dir.pwd
          concurrent += 1
          max_concurrent = concurrent if concurrent > max_concurrent
        end
        File.write "two.txt", "2"
        inside_mutex.synchronize { concurrent -= 1 }
      end
    end

    multitask parallel_in_dir: [:one, :two]

    Rake::Task[:parallel_in_dir].invoke

    assert File.exist?(File.join("one dir", "one.txt"))
    assert File.exist?(File.join("two dir", "two.txt"))
    refute File.exist?("one.txt")
    refute File.exist?("two.txt")
    assert_equal original, Dir.pwd

    assert_equal File.expand_path("one dir"), inside[:one]
    assert_equal File.expand_path("two dir"), inside[:two]
    assert_equal 1, max_concurrent,
      "the process wide chdir blocks must never run at the same time"
  end
end
