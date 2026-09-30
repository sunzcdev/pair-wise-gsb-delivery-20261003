# frozen_string_literal: true

require_relative "../helper"

class TestCSVColumnConversionPlan < Test::Unit::TestCase
  def test_same_header_text_different_conversions
    plan = CSV::ColumnConversionPlan.new
    plan.convert(["amount", 1], with: :integer)
    plan.convert(["amount", 2], with: :float)
    csv = CSV.new("currency,amount,amount\nUSD,1,1.5\nJPY,2,2.5\n",
                  headers: true,
                  column_conversion_plan: plan)
    rows = csv.read
    assert_equal([["USD", 1, 1.5], ["JPY", 2, 2.5]],
                 rows.map(&:fields))
  end

  def test_call_order_and_counts
    calls = []
    strip = lambda {|field| calls << [:strip, field]; field.strip }
    upcase = lambda {|field| calls << [:upcase, field]; field.upcase }
    integer = lambda {|field| calls << [:integer, field]; Integer(field) }
    plan = CSV::ColumnConversionPlan.new
    plan.convert(["amount", 1], with: [strip, integer])
    plan.convert(["amount", 2], with: upcase)
    csv = CSV.new("amount,amount\n 1 ,usd\n 2 ,jpy\n",
                  headers: true,
                  column_conversion_plan: plan)
    rows = csv.read
    assert_equal([[1, "USD"], [2, "JPY"]], rows.map(&:fields))
    assert_equal([
                   [:strip, " 1 "],
                   [:integer, "1"],
                   [:upcase, "usd"],
                   [:strip, " 2 "],
                   [:integer, "2"],
                   [:upcase, "jpy"],
                 ],
                 calls)
  end

  def test_index_selector
    plan = CSV::ColumnConversionPlan.new({1 => :integer})
    csv = CSV.new("a,1\nb,2\n", column_conversion_plan: plan)
    assert_equal([["a", 1], ["b", 2]], csv.read)
  end

  def test_negative_index_selector
    plan = CSV::ColumnConversionPlan.new({-1 => :integer})
    csv = CSV.new("name,amount\na,1\n",
                  headers: true,
                  column_conversion_plan: plan)
    assert_equal([["a", 1]], csv.read.map(&:fields))
  end

  def test_hash_selector
    plan = CSV::ColumnConversionPlan.new
    plan.convert({name: "amount", occurrence: 2}, with: :float)
    csv = CSV.new("amount,amount\n1,1.5\n",
                  headers: true,
                  column_conversion_plan: plan)
    assert_equal([["1", 1.5]], csv.read.map(&:fields))
  end

  def test_occurrence_keyword
    plan = CSV::ColumnConversionPlan.new
    plan.convert("amount", occurrence: 2, with: :float)
    csv = CSV.new("amount,amount\n1,1.5\n",
                  headers: true,
                  column_conversion_plan: plan)
    assert_equal([["1", 1.5]], csv.read.map(&:fields))
  end

  def test_declarative_spec_option
    csv = CSV.new("amount,amount\n1,1.5\n",
                  headers: true,
                  column_conversion_plan: {
                    ["amount", 1] => :integer,
                    ["amount", 2] => :float,
                  })
    assert_equal([[1, 1.5]], csv.read.map(&:fields))
  end

  def test_combo_converter
    plan = CSV::ColumnConversionPlan.new({0 => :numeric})
    csv = CSV.new("1\n1.5\nx\n", column_conversion_plan: plan)
    assert_equal([[1], [1.5], ["x"]], csv.read)
  end

  def test_field_info_convention
    infos = []
    spy = lambda do |field, info|
      infos << info
      field
    end
    plan = CSV::ColumnConversionPlan.new({["amount", 2] => spy})
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    csv.read
    assert_equal(1, infos.size)
    info = infos.first
    assert_equal(1, info.index)
    assert_equal(2, info.line)
    assert_equal("amount", info.header)
  end

  def test_missing_column_name
    plan = CSV::ColumnConversionPlan.new({"total" => :integer})
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_include(error.message, "no column named")
    assert_include(error.message, "total")
  end

  def test_missing_column_index
    plan = CSV::ColumnConversionPlan.new({5 => :integer})
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_include(error.message, "no column at index 5")
  end

  def test_missing_column_occurrence
    plan = CSV::ColumnConversionPlan.new({["amount", 3] => :integer})
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_include(error.message, "no column 3")
  end

  def test_ambiguous_header_name
    plan = CSV::ColumnConversionPlan.new({"amount" => :integer})
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_include(error.message, "ambiguous")
    assert_include(error.message, "amount")
  end

  def test_overlapping_entries
    plan = CSV::ColumnConversionPlan.new
    plan.convert(0, with: :integer)
    plan.convert(["amount", 1], with: :float)
    csv = CSV.new("amount,amount\n1,2\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_include(error.message, "overlapping")
  end

  def test_plan_checked_before_data_rows
    converted = []
    spy = lambda do |field|
      converted << field
      field
    end
    plan = CSV::ColumnConversionPlan.new({"missing" => :integer})
    csv = CSV.new("amount\n1\n2\n",
                  headers: true,
                  converters: spy,
                  column_conversion_plan: plan)
    assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
    assert_equal([], converted)
  end

  def test_name_selector_without_headers
    plan = CSV::ColumnConversionPlan.new({"amount" => :integer})
    csv = CSV.new("1,2\n", column_conversion_plan: plan)
    assert_raise(CSV::InvalidColumnConversionPlanError) do
      csv.shift
    end
  end

  def test_conversion_error_raise
    pass = lambda {|field| field }
    boom = lambda {|_field| raise ArgumentError, "boom" }
    plan = CSV::ColumnConversionPlan.new({["amount", 2] => [pass, boom]})
    csv = CSV.new("amount,amount\n1,2\n3,4\n",
                  headers: true,
                  column_conversion_plan: plan)
    error = assert_raise(CSV::ColumnConversionError) do
      csv.read
    end
    assert_equal(2, error.lineno)
    assert_equal(1, error.column_index)
    assert_equal("amount", error.header)
    assert_equal(2, error.occurrence)
    assert_equal("2", error.original_value)
    assert_equal(1, error.step)
    assert_equal(boom, error.converter)
    assert_instance_of(ArgumentError, error.cause)
    assert_include(error.message, "column 1")
    assert_include(error.message, "amount")
    assert_include(error.message, "occurrence 2")
    assert_include(error.message, "step 1")
    assert_include(error.message, "line 2")
  end

  def test_conversion_error_collect
    boom = lambda do |field|
      raise ArgumentError, "boom" if field == "bad"
      Integer(field)
    end
    plan = CSV::ColumnConversionPlan.new({["amount", 1] => boom},
                                         on_error: :collect)
    csv = CSV.new("amount,amount\n1,keep\nbad,keep\n3,keep\n",
                  headers: true,
                  column_conversion_plan: plan)
    rows = csv.read
    assert_equal([[1, "keep"], ["bad", "keep"], [3, "keep"]],
                 rows.map(&:fields))
    failures = csv.column_conversion_plan.failures
    assert_equal(1, failures.size)
    failure = failures.first
    assert_instance_of(CSV::ColumnConversionError, failure)
    assert_equal(3, failure.lineno)
    assert_equal(0, failure.column_index)
    assert_equal("amount", failure.header)
    assert_equal(1, failure.occurrence)
    assert_equal("bad", failure.original_value)
    assert_equal(0, failure.step)
    assert_equal(boom, failure.converter)
    assert_instance_of(ArgumentError, failure.cause)
  end

  def test_conversion_error_callable
    failures = []
    boom = lambda {|_field| raise ArgumentError, "boom" }
    plan = CSV::ColumnConversionPlan.new(
      {0 => boom},
      on_error: lambda {|failure| failures << failure }
    )
    csv = CSV.new("1\n2\n", column_conversion_plan: plan)
    assert_equal([["1"], ["2"]], csv.read)
    assert_equal(2, failures.size)
    assert_equal([1, 2], failures.map(&:lineno))
  end

  def test_unknown_converter_name
    assert_raise(ArgumentError) do
      CSV::ColumnConversionPlan.new({0 => :no_such_converter})
    end
  end

  def test_invalid_on_error
    assert_raise(ArgumentError) do
      CSV::ColumnConversionPlan.new(on_error: :ignore)
    end
  end
end
