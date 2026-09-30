# frozen_string_literal: true

require_relative "helper"

class TestCSVFieldPresence < Test::Unit::TestCase
  extend DifferentOFS

  HEADERS = %w[a b c]

  def parse_rows(data, converters: nil)
    CSV.parse(data,
              headers: true,
              field_presence: true,
              converters: converters)
  end

  def write_row(row, **options)
    CSV.generate_line(row, field_presence: true, row_sep: "\n", **options)
  end

  def test_similar_rows_keep_values_states_positions_and_bytes
    converter = lambda do |value|
      value == "null" ? nil : value
    end
    data = <<~CSV
      a,b,c
      x
      x,
      x,""
      x,null
    CSV

    rows = parse_rows(data, converters: converter)

    assert_equal(["x", nil, nil], rows[0].fields)
    assert_equal([:field, :missing, :missing], rows[0].field_states)
    assert_equal(0, rows[0].field_position("a"))
    assert_nil(rows[0].field_position("b"))
    assert_nil(rows[0].field_position("c"))
    assert_equal(CSV::Row::FieldPresence.new(0, 0, :field),
                 rows[0].field_presence("a"))
    assert_equal([true, false, false],
                 HEADERS.map { |header| rows[0].field_present?(header) })
    assert_equal("x\n", write_row(rows[0]))

    assert_equal(["x", nil, nil], rows[1].fields)
    assert_equal([:field, :unquoted_empty, :missing], rows[1].field_states)
    assert_equal(1, rows[1].field_position("b"))
    assert_nil(rows[1].field_position("c"))
    assert_equal("x,\n", write_row(rows[1]))

    assert_equal(["x", "", nil], rows[2].fields)
    assert_equal([:field, :quoted_empty, :missing], rows[2].field_states)
    assert_equal(1, rows[2].field_position("b"))
    assert_nil(rows[2].field_position("c"))
    assert_equal("x,\"\"\n", write_row(rows[2]))

    assert_equal(["x", nil, nil], rows[3].fields)
    assert_equal([:field, :converted_nil, :missing], rows[3].field_states)
    assert_equal("x,\n", write_row(rows[3]))
  end

  def test_field_reading_remains_compatible_with_padded_headers
    row = parse_rows("a,b,c\nx").first

    assert_nil(row[1])
    assert_nil(row["b"])
    assert_nil(row.field(2))
    assert_equal([["a", "x"], ["b", nil], ["c", nil]], row.to_a)
  end

  def test_disabled_presence_does_not_attach_parser_states
    row = CSV.parse_line("x", headers: HEADERS, field_presence: false)

    assert_nil(row.field_states)
    assert_equal(["x", nil, nil], row.fields)
    assert_equal("x,,\n", CSV.generate_line(row, row_sep: "\n"))
  end

  def test_direct_rows_record_manual_field_states
    row = CSV::Row.new(HEADERS, ["x", nil, ""])

    assert_equal([:field, :unquoted_empty, :quoted_empty], row.field_states)
    assert_equal("x,,\"\"\n", write_row(row))
  end

  def test_assignment_states
    row = CSV::Row.new(HEADERS, ["x"])

    row["b"] = nil
    assert_equal(:unquoted_empty, row.field_state("b"))

    row["c"] = ""
    assert_equal(:quoted_empty, row.field_state("c"))

    row["c"] = "z"
    assert_equal(:field, row.field_state("c"))

    assert_equal("x,,z\n", write_row(row))
  end

  def test_index_extension_marks_gaps_missing
    row = CSV::Row.new(HEADERS, [])

    row[2] = "z"

    assert_equal([:missing, :missing, :field], row.field_states)
    assert_equal(",,z\n", write_row(row))
  end

  def test_append_and_delete_update_states
    row = CSV::Row.new(HEADERS, ["x", nil, ""])

    row << ["d", nil]
    row << "e"
    assert_equal([:field, :unquoted_empty, :quoted_empty,
                  :unquoted_empty, :field],
                 row.field_states)

    row.delete("d")
    assert_equal([:field, :unquoted_empty, :quoted_empty, :field],
                 row.field_states)

    row.delete_if { |header, _value| header == "c" }
    assert_equal([:field, :unquoted_empty, :field],
                 row.field_states)
  end

  def test_quoted_empty_is_quoted_even_when_quote_empty_is_false
    row = parse_rows("a,b,c\nx,\"\"").first

    assert_equal("x,\"\"\n",
                 write_row(row, quote_empty: false))
  end

  def test_quote_character_nil_records_present_unquoted_empty_fields
    row = CSV.parse("x\nx,\n",
                    headers: HEADERS,
                    field_presence: true,
                    quote_char: nil)[1]

    assert_equal([:field, :unquoted_empty, :missing], row.field_states)
    assert_equal("x,\n", write_row(row))
  end

  def test_presence_for_duplicate_headers_and_missing_actual_position
    data = <<~CSV
      a,b,a
      x
      x,y
    CSV
    parsed_rows = parse_rows(data)
    first = parsed_rows[0]
    second = parsed_rows[1]

    assert_equal(CSV::Row::FieldPresence.new(0, 0, :field),
                 first.field_presence("a"))
    assert_equal(CSV::Row::FieldPresence.new(1, nil, :missing),
                 first.field_presence("b"))
    assert_equal(CSV::Row::FieldPresence.new(2, nil, :missing),
                 first.field_presence("a", 1))

    assert_equal(CSV::Row::FieldPresence.new(1, 1, :field),
                 second.field_presence("b"))
    assert_equal(CSV::Row::FieldPresence.new(2, nil, :missing),
                 second.field_presence("a", 1))
  end

  def test_copy_keeps_independent_states
    row = parse_rows("a,b,c\nx").first.dup

    row["b"] = ""

    assert_equal(:quoted_empty, row.field_state("b"))
    assert_equal(:missing, parse_rows("a,b,c\nx").first.field_state("b"))
  end

  def test_static_values_do_not_change_original_states
    row = CSV.parse_line("x,,\"\"",
                         headers: HEADERS,
                         field_presence: true,
                         nil_value: "nil",
                         empty_value: "empty")

    assert_equal(["x", "nil", "empty"], row.fields)
    assert_equal([:field, :unquoted_empty, :quoted_empty],
                 row.field_states)
  end

  def test_missing_fields_are_removed_before_write_converters
    row = parse_rows("a,b,c\nx").first

    assert_equal("x\n",
                 write_row(row, write_nil_value: "missing"))
  end

  def test_converted_configured_nil_value_keeps_original_state
    row = CSV.parse_line("x,",
                         headers: HEADERS,
                         field_presence: true,
                         nil_value: "null",
                         converters: lambda { |value|
                           value == "null" ? nil : value
                         })

    assert_equal(["x", nil, nil], row.fields)
    assert_equal([:field, :unquoted_empty, :missing],
                 row.field_states)
  end

  def test_converted_configured_empty_value_keeps_original_state
    row = CSV.parse_line("x,,\"\"",
                         headers: HEADERS,
                         field_presence: true,
                         empty_value: "empty",
                         converters: lambda { |value|
                           value == "empty" ? nil : value
                         })

    assert_equal([:field, :unquoted_empty, :quoted_empty],
                 row.field_states)
  end

  def test_filter_preserves_similar_rows_without_extra_trailing_commas
    input = <<~CSV
      a,b,c
      x
      x,
      x,""
    CSV
    output = +""

    CSV.filter(input, output,
               headers: true,
               field_presence: true,
               write_headers: true,
               row_sep: "\n") do |row|
      next if row.is_a?(Array)
      row["a"].upcase!
    end

    expected = <<~CSV
      a,b,c
      X
      X,
      X,""
    CSV
    assert_equal(expected, output)
  end

  def test_table_to_csv_preserves_similar_rows
    table = parse_rows(<<~CSV)
      a,b,c
      x
      x,
      x,""
    CSV

    assert_equal(<<~CSV, table.to_csv(field_presence: true, row_sep: "\n"))
      a,b,c
      x
      x,
      x,""
    CSV
  end
end
