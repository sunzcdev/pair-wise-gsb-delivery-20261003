# frozen_string_literal: true

require_relative "helper"

class TestCSVMultisetDiff < Test::Unit::TestCase
  def rows(array)
    array.map { |key, value| {"id" => key, "value" => value} }
  end

  def make_diff(max_group_size: 3)
    CSV::MultisetDiff.new(
      key: ->(row) { row["id"] },
      content: ->(row) { row["value"] },
      max_group_size: max_group_size,
    )
  end

  def collect(diff, left, right)
    differences = []
    diff.each(left, right) { |difference| differences << difference }
    differences
  end

  def test_identical_inputs_have_no_differences
    diff = make_diff
    left = rows([["a", 1], ["a", 1], ["b", 2]])
    right = rows([["a", 1], ["a", 1], ["b", 2]])
    assert_equal([], collect(diff, left, right))
  end

  def test_extra_identical_row_on_one_side
    diff = make_diff
    left = rows([["a", 1], ["a", 1], ["a", 1], ["b", 2]])
    right = rows([["a", 1], ["a", 1], ["b", 2]])
    differences = collect(diff, left, right)
    assert_equal(1, differences.size)
    difference = differences[0]
    assert_equal(:left_only, difference.side)
    assert_equal("a", difference.key)
    assert_equal({"id" => "a", "value" => 1}, difference.row)
  end

  def test_extra_identical_row_on_right_side
    diff = make_diff
    left = rows([["a", 1]])
    right = rows([["a", 1], ["a", 1]])
    differences = collect(diff, left, right)
    assert_equal([:right_only], differences.map(&:side))
  end

  def test_same_key_different_content_is_not_a_modification
    diff = make_diff
    left = rows([["a", 1], ["b", 2]])
    right = rows([["a", 1], ["b", 3]])
    differences = collect(diff, left, right)
    assert_equal([:left_only, :right_only],
                 differences.map(&:side).sort_by(&:to_s))
    assert_equal(["b", "b"], differences.map(&:key))
    by_side = differences.group_by(&:side)
    assert_equal({"id" => "b", "value" => 2}, by_side[:left_only][0].row)
    assert_equal({"id" => "b", "value" => 3}, by_side[:right_only][0].row)
  end

  def test_disjoint_keys
    diff = make_diff
    left = rows([["a", 1], ["c", 3]])
    right = rows([["b", 2], ["d", 4]])
    differences = collect(diff, left, right)
    assert_equal([[:left_only, "a"], [:right_only, "b"],
                  [:left_only, "c"], [:right_only, "d"]],
                 differences.map { |d| [d.side, d.key] }.sort_by(&:last))
  end

  def test_group_exceeding_max_group_size_spills_and_pairs
    diff = make_diff(max_group_size: 3)
    left = rows([["k", 1]] * 5 + [["k", 2]] * 4 + [["k", 3]])
    right = rows([["k", 1]] * 5 + [["k", 2]] * 2 + [["k", 4]])
    spilled = false
    spy = lambda do |difference|
      spilled ||= !diff.live_tempfiles.empty?
    end
    differences = []
    diff.each(left, right) do |difference|
      spy.call(difference)
      differences << difference
    end
    assert do
      spilled
    end
    assert_equal([], diff.live_tempfiles)
    assert_equal([[:left_only, 2], [:left_only, 2],
                  [:left_only, 3],
                  [:right_only, 4]],
                 differences.map { |d| [d.side, d.row["value"]] })
  end

  def test_spilled_group_with_many_identical_rows
    diff = make_diff(max_group_size: 2)
    left = rows([["k", 1]] * 9)
    right = rows([["k", 1]] * 7)
    differences = collect(diff, left, right)
    assert_equal([[:left_only, 1], [:left_only, 1]],
                 differences.map { |d| [d.side, d.row["value"]] })
    assert_equal([], diff.live_tempfiles)
  end

  def test_unordered_left_input_reports_position
    diff = make_diff
    left = rows([["a", 1], ["c", 3], ["b", 2]])
    right = rows([["a", 1]])
    error = assert_raise(CSV::MultisetDiff::UnorderedInputError) do
      collect(diff, left, right)
    end
    assert_equal(:left, error.side)
    assert_equal(3, error.position)
    assert_equal("c", error.previous_key)
    assert_equal("b", error.key)
    assert_match(/record 3/, error.message)
  end

  def test_unordered_right_input_reports_position
    diff = make_diff
    left = rows([["a", 1]])
    right = rows([["b", 2], ["a", 1]])
    error = assert_raise(CSV::MultisetDiff::UnorderedInputError) do
      collect(diff, left, right)
    end
    assert_equal(:right, error.side)
    assert_equal(2, error.position)
  end

  def test_early_termination_cleans_up_tempfiles
    diff = make_diff(max_group_size: 2)
    left = rows([["k", 1]] * 6 + [["z", 9]])
    right = rows([["k", 2]] * 6)
    consumed = []
    assert_raise(RuntimeError) do
      diff.each(left, right) do |difference|
        consumed << difference
        raise "consumer aborted"
      end
    end
    assert_equal(1, consumed.size)
    assert_equal([], diff.live_tempfiles)
  end

  def test_early_termination_via_enumerator_take
    diff = make_diff(max_group_size: 2)
    left = rows([["k", 1]] * 6)
    right = rows([["k", 2]] * 6)
    first = diff.each(left, right).first(2)
    assert_equal(2, first.size)
    GC.start
    assert_equal([], diff.live_tempfiles)
  end

  def test_returns_enumerator_without_block
    diff = make_diff
    enum = diff.each(rows([["a", 1]]), rows([["a", 1], ["a", 1]]))
    assert_instance_of(Enumerator, enum)
    assert_equal([:right_only], enum.map(&:side))
  end

  def test_max_group_size_must_be_positive
    assert_raise(ArgumentError) do
      CSV::MultisetDiff.new(key: ->(row) { row }, max_group_size: 0)
    end
  end

  def test_works_with_csv_instances
    diff = CSV::MultisetDiff.new(
      key: ->(row) { row["id"] },
      content: ->(row) { row.fields },
      max_group_size: 2,
    )
    left = CSV.new("id,v\n1,a\n2,b\n", headers: true)
    right = CSV.new("id,v\n1,a\n2,c\n", headers: true)
    differences = []
    diff.each(left.each, right.each) { |d| differences << d }
    assert_equal([:left_only, :right_only],
                 differences.map(&:side).sort_by(&:to_s))
    assert_equal(["2", "2"], differences.map(&:key))
  end
end
