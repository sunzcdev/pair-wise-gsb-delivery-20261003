# frozen_string_literal: true

require_relative "helper"

class TestCSVMultisetDiff < Test::Unit::TestCase
  def diff(left, right, **options)
    CSV::MultisetDiff.new(left, right,
                          key: ->(row) { row[0] },
                          content: ->(row) { row[1..] },
                          **options).to_a
  end

  def test_identical_inputs
    rows = [["a", "1"], ["a", "1"], ["b", "2"]]
    assert_equal([], diff(rows, rows.dup))
  end

  def test_one_extra_identical_row_on_left
    left = [["a", "1"], ["a", "1"], ["a", "1"]]
    right = [["a", "1"], ["a", "1"]]
    assert_equal([[:left_only, "a", ["a", "1"]]], diff(left, right))
  end

  def test_one_extra_identical_row_on_right
    left = [["a", "1"]]
    right = [["a", "1"], ["a", "1"]]
    assert_equal([[:right_only, "a", ["a", "1"]]], diff(left, right))
  end

  def test_same_key_different_content_is_not_a_modification
    left = [["a", "old"]]
    right = [["a", "new"]]
    assert_equal([[:left_only, "a", ["a", "old"]],
                  [:right_only, "a", ["a", "new"]]],
                 diff(left, right).sort)
  end

  def test_pairing_is_by_count_within_a_key_group
    left = [["a", "x"], ["a", "x"], ["a", "y"]]
    right = [["a", "x"], ["a", "y"], ["a", "y"]]
    assert_equal([[:left_only, "a", ["a", "x"]],
                  [:right_only, "a", ["a", "y"]]],
                 diff(left, right).sort)
  end

  def test_keys_present_on_only_one_side
    left = [["a", "1"], ["c", "3"]]
    right = [["b", "2"], ["d", "4"]]
    assert_equal([[:left_only, "a", ["a", "1"]],
                  [:right_only, "b", ["b", "2"]],
                  [:left_only, "c", ["c", "3"]],
                  [:right_only, "d", ["d", "4"]]],
                 diff(left, right))
  end

  def test_empty_inputs
    assert_equal([], diff([], []))
    assert_equal([[:right_only, "a", ["a", "1"]]], diff([], [["a", "1"]]))
    assert_equal([[:left_only, "a", ["a", "1"]]], diff([["a", "1"]], []))
  end

  def test_csv_inputs
    left = CSV.new("a,1\na,1\nb,2\n")
    right = CSV.new("a,1\nb,2\nb,3\n")
    events = CSV::MultisetDiff.new(left, right,
                                   key: ->(row) { row[0] },
                                   content: ->(row) { row.to_a }).to_a
    assert_equal([[:left_only, "a", ["a", "1"]],
                  [:right_only, "b", ["b", "3"]]],
                 events.map { |event, key, row| [event, key, row.to_a] })
  end

  def test_unsorted_left_input_reports_record_position
    left = [["a", "1"], ["c", "3"], ["b", "2"]]
    right = []
    error = assert_raise(CSV::UnsortedInputError) do
      diff(left, right)
    end
    assert_equal(:left, error.side)
    assert_equal(3, error.position)
    assert_equal("b", error.key)
    assert_equal("c", error.previous_key)
    assert_include(error.message, "record 3")
  end

  def test_unsorted_right_input_reports_record_position
    left = []
    right = [["b", "2"], ["a", "1"]]
    error = assert_raise(CSV::UnsortedInputError) do
      diff(left, right)
    end
    assert_equal(:right, error.side)
    assert_equal(2, error.position)
    assert_include(error.message, "right input is not sorted")
  end

  def test_group_larger_than_max_group_size_spills_to_tempfile
    left = (1..10).map {|i| ["a", "value-#{i}"]} +
           (1..4).map {|i| ["a", "left-only-#{i}"]}
    right = (1..10).map {|i| ["a", "value-#{i}"]} +
            (1..3).map {|i| ["a", "right-only-#{i}"]}
    events = diff(left, right, max_group_size: 4)
    assert_equal((1..4).map {|i| [:left_only, "a", ["a", "left-only-#{i}"]]} +
                 (1..3).map {|i| [:right_only, "a", ["a", "right-only-#{i}"]]},
                 events)
  end

  def test_spilled_group_pairs_duplicates_by_count
    left = (1..6).map {["a", "same"]} + [["a", "other"]]
    right = (1..4).map {["a", "same"]} + [["a", "other"]]
    events = diff(left, right, max_group_size: 2)
    assert_equal([[:left_only, "a", ["a", "same"]],
                  [:left_only, "a", ["a", "same"]]],
                 events)
  end

  def test_spilled_group_with_many_distinct_contents
    left = (1..20).map {|i| ["k", "content-%02d" % i]}
    right = (11..30).map {|i| ["k", "content-%02d" % i]}
    events = diff(left, right, max_group_size: 3)
    assert_equal((1..10).map {|i| [:left_only, "k", ["k", "content-%02d" % i]]} +
                 (21..30).map {|i| [:right_only, "k", ["k", "content-%02d" % i]]},
                 events)
  end

  def test_tempfiles_are_removed_after_spilled_group
    leftover_paths = nil
    left = (1..10).map {|i| ["a", "value-#{i}"]}
    right = (1..10).map {|i| ["a", "value-#{i}"]}
    Tempfile.create(["csv-multiset-diff-probe", ".run"]) do |probe|
      dir = File.dirname(probe.path)
      before = Dir.glob(File.join(dir, "csv-multiset-diff*.run"))
      assert_equal([], diff(left, right, max_group_size: 2))
      leftover_paths = Dir.glob(File.join(dir, "csv-multiset-diff*.run")) - before
    end
    assert_equal([], leftover_paths)
  end

  def test_early_break_cleans_up_spilled_group_tempfiles
    leftover_paths = nil
    left = (1..10).map {|i| ["a", "left-#{i}"]}
    right = (1..10).map {|i| ["a", "right-#{i}"]}
    Tempfile.create(["csv-multiset-diff-probe", ".run"]) do |probe|
      dir = File.dirname(probe.path)
      before = Dir.glob(File.join(dir, "csv-multiset-diff*.run"))
      diff_enum = CSV::MultisetDiff.new(left, right,
                                        key: ->(row) { row[0] },
                                        content: ->(row) { row[1..] },
                                        max_group_size: 2)
      diff_enum.each do |_event|
        break
      end
      leftover_paths = Dir.glob(File.join(dir, "csv-multiset-diff*.run")) - before
    end
    assert_equal([], leftover_paths)
  end

  def test_early_break_before_any_group
    left = [["a", "1"], ["b", "2"]]
    right = [["a", "1"], ["b", "2"]]
    yielded = []
    CSV::MultisetDiff.new(left, right, key: ->(row) { row[0] }).each do |event|
      yielded << event
      break
    end
    assert_equal([], yielded)
  end

  def test_invalid_max_group_size
    assert_raise(ArgumentError) do
      CSV::MultisetDiff.new([], [],
                            key: ->(row) { row[0] },
                            max_group_size: 0)
    end
  end

  def test_lazy_enumeration
    consumed = 0
    left = Enumerator.new do |yielder|
      [["a", "1"], ["b", "2"]].each do |row|
        consumed += 1
        yielder << row
      end
    end
    enum = CSV::MultisetDiff.new(left, [],
                                 key: ->(row) { row[0] }).each
    assert_equal(0, consumed)
    enum.next
    assert_equal(1, consumed)
  end
end
