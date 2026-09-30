# -*- coding: utf-8 -*-
# frozen_string_literal: false

require_relative "../helper"

class TestCSVParseColumnConverters < Test::Unit::TestCase
  extend DifferentOFS

  def setup
    super
    @data = "amount,amount\n10,10\n20,20\n"
  end

  # Two columns share the same header text and the same field values, but
  # each has its own conversion chain.
  def test_duplicate_headers_use_different_chains
    original = lambda {|field| "orig:#{field}" }
    settlement = lambda {|field| "sett:#{field}" }
    table = CSV.parse(@data,
                      headers: true,
                      column_converters: {
                        ["amount", 1] => original,
                        ["amount", 2] => settlement,
                      })
    assert_equal([["orig:10", "sett:10"], ["orig:20", "sett:20"]],
                 table.map {|row| row.fields})
  end

  def test_duplicate_headers_with_hash_selectors
    original = lambda {|field| "orig:#{field}" }
    settlement = lambda {|field| "sett:#{field}" }
    table = CSV.parse(@data,
                      headers: true,
                      column_converters: {
                        {header: "amount", occurrence: 1} => original,
                        {header: "amount", occurrence: 2} => settlement,
                      })
    assert_equal([["orig:10", "sett:10"], ["orig:20", "sett:20"]],
                 table.map {|row| row.fields})
  end

  def test_column_converters_are_called_in_order_and_count
    log = []
    global_one = lambda {|field| log << [:global1, field]; field }
    global_two = lambda {|field| log << [:global2, field]; field }
    original = lambda {|field| log << [:original, field]; field }
    settlement = lambda {|field| log << [:settlement, field]; field }

    CSV.parse(@data,
              headers: true,
              converters: [global_one, global_two],
              column_converters: {
                ["amount", 1] => original,
                ["amount", 2] => settlement,
              })

    # Global converters run before the column chain, and each converter is
    # called once per matching cell.
    assert_equal([
                   [:global1, "10"], [:global2, "10"], [:original, "10"],
                   [:global1, "10"], [:global2, "10"], [:settlement, "10"],
                   [:global1, "20"], [:global2, "20"], [:original, "20"],
                   [:global1, "20"], [:global2, "20"], [:settlement, "20"],
                 ],
                 log)
  end

  def test_column_chain_stops_when_value_is_no_longer_a_string
    log = []
    integer = lambda {|field| log << :integer; Integer(field) }
    never = lambda {|field| log << :never; flunk("must not be called"); field }

    table = CSV.parse("n\n1\n",
                      headers: true,
                      column_converters: {0 => [integer, never]})
    assert_equal([1], table.first.fields)
    assert_equal([:integer], log)
  end

  def test_builtin_converter_names_are_supported
    table = CSV.parse("a,b\n1,2.5\n",
                      headers: true,
                      column_converters: {
                        "a" => :integer,
                        "b" => :float,
                      })
    assert_equal([1, 2.5], table.first.fields)
  end

  def test_combo_converter_name_expands_into_steps
    table = CSV.parse("a,b\n1,2.5\n",
                      headers: true,
                      column_converters: {0 => :numeric})
    assert_equal([1, "2.5"], table.first.fields)
  end

  def test_field_info_is_passed_to_arity_two_converters
    infos = []
    recorder = lambda do |field, info|
      infos << [info.index, info.line, info.header]
      field
    end
    CSV.parse("amount,amount\n10,20\n",
              headers: true,
              column_converters: {["amount", 2] => recorder})
    assert_equal([[1, 2, "amount"]], infos)
  end

  def test_index_selectors_without_headers
    rows = CSV.parse("1,2.5,x\n",
                     column_converters: {
                       0 => :integer,
                       1 => :float,
                     })
    assert_equal([[1, 2.5, "x"]], rows)
  end

  def test_negative_index_selector
    rows = CSV.parse("1,2,3\n",
                     column_converters: {-1 => :integer})
    assert_equal([["1", "2", 3]], rows)
  end

  def test_range_selector
    rows = CSV.parse("1,2,3\n",
                     column_converters: {0..1 => :integer})
    assert_equal([[1, 2, "3"]], rows)
  end

  def test_hash_index_selector
    rows = CSV.parse("1,2\n",
                     column_converters: {{index: 1} => :integer})
    assert_equal([["1", 2]], rows)
  end

  def test_missing_column_by_name_raises_after_header_is_read
    error = assert_raise(CSV::MissingColumnError) do
      CSV.parse("a,b\n1,2\n",
                headers: true,
                column_converters: {"c" => :integer})
    end
    assert_match(/column not found: "c"/, error.message)
  end

  def test_ambiguous_header_name_raises_after_header_is_read
    error = assert_raise(CSV::AmbiguousColumnError) do
      CSV.parse("a,a\n1,2\n",
                headers: true,
                column_converters: {"a" => :integer})
    end
    assert_match(/ambiguous/, error.message)
  end

  def test_missing_occurrence_raises_after_header_is_read
    assert_raise(CSV::MissingColumnError) do
      CSV.parse("a,a\n1,2\n",
                headers: true,
                column_converters: {["a", 3] => :integer})
    end
  end

  def test_overlapping_selectors_raise_after_header_is_read
    error = assert_raise(CSV::DuplicateColumnError) do
      CSV.parse("a,b\n1,2\n",
                headers: true,
                column_converters: {
                  0 => :integer,
                  ["a", 1] => :float,
                })
    end
    assert_match(/index 0/, error.message)
  end

  def test_overlapping_range_selectors_raise
    assert_raise(CSV::DuplicateColumnError) do
      CSV.parse("1,2,3\n",
                column_converters: {
                  0..1 => :integer,
                  1 => :float,
                })
    end
  end

  def test_header_errors_fire_before_business_data_is_processed
    seen_data = false
    assert_raise(CSV::MissingColumnError) do
      CSV.foreach(StringIO.new("a,b\n1,2\n3,4\n"),
                  headers: true,
                  column_converters: {"c" => :integer}) do |_row|
        seen_data = true
      end
    end
    assert_false(seen_data)
  end

  def test_header_array_validates_when_the_csv_object_is_built
    assert_raise(CSV::MissingColumnError) do
      CSV.new("1,2\n",
              headers: ["a", "b"],
              column_converters: {"c" => :integer}).to_a
    end
  end

  def test_name_selector_without_headers_raises
    assert_raise(CSV::MissingColumnError) do
      CSV.parse("1,2\n",
                column_converters: {"a" => :integer})
    end
  end

  def test_index_out_of_range_raises_on_first_row
    assert_raise(CSV::MissingColumnError) do
      CSV.parse("1,2\n",
                column_converters: {2 => :integer})
    end
  end

  def test_invalid_selector_raises_argument_error
    assert_raise(ArgumentError) do
      CSV.new("a\n1\n",
              headers: true,
              column_converters: {Object.new => :integer})
    end
  end

  def test_unknown_converter_name_raises_argument_error
    error = assert_raise(ArgumentError) do
      CSV.new("a\n1\n",
              headers: true,
              column_converters: {0 => :nosuch})
    end
    assert_match(/unknown converter/, error.message)
  end

  def test_empty_converter_chain_raises_argument_error
    assert_raise(ArgumentError) do
      CSV.new("a\n1\n",
              headers: true,
              column_converters: {0 => []})
    end
  end

  def test_invalid_error_mode_raises_argument_error
    assert_raise(ArgumentError) do
      CSV.new("a\n1\n",
              headers: true,
              column_converters: {0 => :integer},
              column_conversion_errors: :bogus)
    end
  end

  def test_conversion_failure_raises_by_default_with_full_context
    failing = lambda {|field| raise "cannot parse #{field}" }
    error = assert_raise(CSV::ColumnConversionError) do
      CSV.parse("amount,amount\n10,bad\n",
                headers: true,
                column_converters: {["amount", 2] => failing})
    end
    failure = error.failure
    assert_equal(1, failure.index)
    assert_equal("amount", failure.header)
    assert_equal(2, failure.occurrence)
    assert_equal(2, failure.lineno)
    assert_equal(1, failure.record)
    assert_equal("bad", failure.field)
    assert_equal(0, failure.step)
    assert_kind_of(RuntimeError, failure.cause)
    assert_match(/line 2/, error.message)
    assert_match(/"bad"/, error.message)
  end

  def test_failure_keeps_original_value_even_after_global_conversion
    global = lambda {|field| field == "bad" ? "GLOBAL" : field }
    failing = lambda {|field| raise "got #{field}" }
    error = assert_raise(CSV::ColumnConversionError) do
      CSV.parse("a,b\n1,bad\n",
                headers: true,
                converters: global,
                column_converters: {1 => failing})
    end
    # The failure reports the value parsed from the CSV, not the value the
    # global converter passed into the column chain.
    assert_equal("bad", error.failure.field)
    assert_match(/got GLOBAL/, error.failure.cause.message)
  end

  def test_conversion_failure_reports_failing_step_in_chain
    failing = lambda {|field| raise "boom" }
    error = assert_raise(CSV::ColumnConversionError) do
      CSV.parse("a\nx\n",
                headers: true,
                column_converters: {0 => [:integer, failing]})
    end
    # :integer leaves non-integer-looking strings alone, so the failing
    # custom Proc is step 1.
    assert_equal(1, error.failure.step)
  end

  def test_collect_mode_keeps_failed_cells_and_attaches_failures
    failing = lambda {|field| raise "boom" }
    table = CSV.parse("a,b,c\n1,x,2\n",
                      headers: true,
                      column_converters: {
                        0 => :integer,
                        1 => failing,
                        2 => :integer,
                      },
                      column_conversion_errors: :collect)
    row = table.first
    assert_equal([1, "x", 2], row.fields)
    failures = row.column_conversion_errors
    assert_equal(1, failures.size)
    assert_equal(1, failures.first.index)
    assert_equal("x", failures.first.field)
  end

  def test_collect_mode_gathers_multiple_failures
    failing = lambda {|field| raise "boom" }
    table = CSV.parse("a,b\nx,y\n",
                      headers: true,
                      column_converters: {
                        0 => failing,
                        1 => failing,
                      },
                      column_conversion_errors: :collect)
    assert_equal([0, 1], table.first.column_conversion_errors.map(&:index))
    assert_equal(["x", "y"],
                 table.first.column_conversion_errors.map(&:field))
  end

  def test_collect_mode_attaches_failures_without_headers
    failing = lambda {|field| raise "boom" }
    rows = []
    CSV.foreach(StringIO.new("1,x\n2,y\n"),
                column_converters: {1 => failing},
                column_conversion_errors: :collect) do |row|
      rows << row
    end
    assert_equal([["1", "x"], ["2", "y"]], rows)
    assert_equal(["x", "y"],
                 rows.map {|row| row.column_conversion_errors.first.field})
    assert_equal([1, 2],
                 rows.map {|row| row.column_conversion_errors.first.record})
  end

  def test_collect_mode_failure_identities_keep_record_numbers
    failing = lambda {|field| raise "boom" if field == "bad"; field }
    table = CSV.parse("a\ngood\nbad\ngood\n",
                      headers: true,
                      column_converters: {0 => failing},
                      column_conversion_errors: :collect)
    failures = table.by_row.flat_map do |row|
      row.column_conversion_errors || []
    end
    assert_equal([2], failures.map(&:record))
    assert_equal([3], failures.map(&:lineno))
  end
end
