# frozen_string_literal: false

require_relative "helper"

class TestCSVFieldPresence < Test::Unit::TestCase
  HEADERS = %w{a b c}
  # Rows that all read back as having three columns, but were provided
  # differently in the source.
  SOURCE = <<~CSV
    a,b,c
    1,2,3
    1,2
    1,2,
    1,2,""
  CSV

  def parse(source, **options)
    CSV.parse(source, headers: true, field_presence: true, **options)
  end

  def rows(source, **options)
    parse(source, **options).each.to_a
  end

  def test_field_values_stay_compatible
    rows = rows(SOURCE)
    assert_equal(["1", "2", "3"], rows[0].fields)
    assert_equal(["1", "2", nil], rows[1].fields)
    assert_equal(["1", "2", nil], rows[2].fields)
    assert_equal(["1", "2", ""],  rows[3].fields)
    assert_equal("3", rows[0]["c"])
    assert_nil(rows[1]["c"])
    assert_nil(rows[2]["c"])
    assert_equal("", rows[3]["c"])
  end

  def test_field_state
    rows = rows(SOURCE)
    assert_equal(:value,        rows[0].field_state("c"))
    assert_equal(:missing,      rows[1].field_state("c"))
    assert_equal(:empty,        rows[2].field_state("c"))
    assert_equal(:quoted_empty, rows[3].field_state("c"))
    rows.each do |row|
      assert_equal(:value, row.field_state("a"))
      assert_equal(:value, row.field_state(1))
    end
  end

  def test_field_state_with_converter_nil
    converter = ->(field) { field == "NULL" ? nil : field }
    rows = rows("a,b,c\n1,2,NULL\n1,2,3\n", converters: converter)
    assert_nil(rows[0]["c"])
    assert_equal(:nil_value, rows[0].field_state("c"))
    assert_equal(:value, rows[1].field_state("c"))
  end

  def test_field_present?
    rows = rows(SOURCE)
    assert do
      rows[0].field_present?("c") and
        not rows[1].field_present?("c") and
        rows[2].field_present?("c") and
        rows[3].field_present?("c")
    end
    assert_equal([true, false, true, true],
                 rows.map {|row| row.field_present?(2) })
  end

  def test_field_position
    row = parse(SOURCE).first
    assert_equal(0, row.field_position("a"))
    assert_equal(2, row.field_position("c"))
    assert_equal(1, row.field_position(1))
    assert_equal(2, row.field_position(-1))
    assert_nil(row.field_position(3))
    assert_nil(row.field_position("nope"))
  end

  def test_untracked_row
    row = CSV.parse(SOURCE, headers: true).first
    assert_nil(row.field_state("c"))
    assert(row.field_present?("c"))
    assert(!row.field_present?(10))
    assert_equal(["1", "2", "3"], row.present_fields)
  end

  def test_present_fields
    rows = rows(SOURCE)
    assert_equal(["1", "2", "3"], rows[0].present_fields)
    assert_equal(["1", "2"],      rows[1].present_fields)
    assert_equal(["1", "2", nil], rows[2].present_fields)
    assert_equal(["1", "2", ""],  rows[3].present_fields)
  end

  def test_row_to_csv
    rows = rows(SOURCE)
    # Default mode stays compatible: nil-filled fields are written.
    assert_equal("1,2,3\n", rows[0].to_csv)
    assert_equal("1,2,\n",  rows[1].to_csv)
    # Write-back mode: no trailing separators appear out of nowhere.
    assert_equal("1,2,3\n",    rows[0].to_csv(field_presence: true))
    assert_equal("1,2\n",      rows[1].to_csv(field_presence: true))
    assert_equal("1,2,\n",     rows[2].to_csv(field_presence: true))
    assert_equal("1,2,\"\"\n", rows[3].to_csv(field_presence: true))
  end

  def test_table_to_csv
    table = parse(SOURCE)
    assert_equal(SOURCE, table.to_csv(field_presence: true))
  end

  def test_csv_shift_and_write
    output = +""
    csv = CSV(output, headers: true, field_presence: true)
    SOURCE.each_line.drop(1).each do |line|
      row = CSV.parse_line(line, headers: HEADERS, field_presence: true)
      csv << row
    end
    assert_equal(SOURCE.lines.drop(1).join, output)
  end

  def test_assignment_marks_value
    row = parse(SOURCE)[1]
    assert_equal(:missing, row.field_state("c"))
    row["c"] = "3"
    assert_equal(:value, row.field_state("c"))
    assert_equal("1,2,3\n", row.to_csv(field_presence: true))

    row = parse(SOURCE)[1]
    row[2] = nil
    assert_equal(:value, row.field_state("c"))
    assert_equal("1,2,\n", row.to_csv(field_presence: true))
  end

  def test_index_extension_marks_gaps_missing
    row = parse(SOURCE)[0]
    row[4] = 5
    assert_equal(:missing, row.field_state(3))
    assert_equal(:value, row.field_state(4))
    assert_equal("1,2,3,,5\n", row.to_csv(field_presence: true))
  end

  def test_append_marks_value
    row = parse(SOURCE)[1]
    row << ["d", "4"]
    assert_equal(:value, row.field_state("d"))
    assert_equal("1,2,,4\n", row.to_csv(field_presence: true))

    row = parse(SOURCE)[1]
    row << "4"
    assert_equal(:value, row.field_state(3))
    assert_equal("1,2,,4\n", row.to_csv(field_presence: true))
  end

  def test_delete_updates_state
    row = parse(SOURCE)[0]
    row.delete("b")
    assert_equal(%w{a c}, row.headers)
    assert_equal(:value, row.field_state("c"))
    assert_nil(row.field_state("b"))
    assert_equal("1,3\n", row.to_csv(field_presence: true))

    row = parse(SOURCE)[0]
    row.delete(0)
    assert_equal(:value, row.field_state(0))
    assert_equal("2,3\n", row.to_csv(field_presence: true))
  end

  def test_delete_if_updates_state
    row = parse(SOURCE)[0]
    row.delete_if {|header, _value| header == "b" }
    assert_equal(%w{a c}, row.headers)
    assert_equal(:value, row.field_state("c"))
    assert_equal("1,3\n", row.to_csv(field_presence: true))
  end

  def test_dup_keeps_state
    row = parse(SOURCE)[1].dup
    assert_equal(:missing, row.field_state("c"))
    row["c"] = "3"
    assert_equal(:value, row.field_state("c"))
  end

  def test_extra_fields
    row = parse("a,b\n1,2,3,\n").first
    assert_equal(["1", "2", "3", nil], row.fields)
    assert_equal(:value, row.field_state(2))
    assert_equal(:empty, row.field_state(3))
    assert_equal("1,2,3,\n", row.to_csv(field_presence: true))
  end

  def test_row_new_with_field_info
    row = CSV::Row.new(%w{a b c}, [1, 2],
                       field_info: [:value, :value, :missing])
    assert_equal(:missing, row.field_state("c"))
    assert(!row.field_present?("c"))
    assert_equal("1,2\n", row.to_csv(field_presence: true))

    # Short info is padded with :missing; long info is truncated.
    row = CSV::Row.new(%w{a b c}, [1, 2], field_info: [:value])
    assert_equal(:missing, row.field_state("b"))
    row = CSV::Row.new(%w{a}, [1], field_info: [:value, :missing])
    assert_equal([["a", 1]], row.to_a)
  end
end
