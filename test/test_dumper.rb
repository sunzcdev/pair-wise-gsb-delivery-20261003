require 'minitest_helper'

describe Tomlrb::Dumper do
  def round_trip(hash)
    Tomlrb.parse(Tomlrb.dump(hash))
  end

  it "round-trips a plain hash of scalars" do
    hash = {
      "title" => "TOML Example",
      "count" => 42,
      "negative" => -17,
      "ratio" => 3.1415,
      "exponent" => 5.0e+22,
      "enabled" => true,
      "disabled" => false,
      "multiline" => "line one\nline two\t\"quoted\" \\ backslash",
      "unicode" => "中国"
    }
    _(round_trip(hash)).must_equal hash
  end

  it "keeps the distinction between integer and float values" do
    hash = {"integer" => 1, "float" => 1.0, "big_float" => 1000000.0}
    parsed = round_trip(hash)
    _(parsed).must_equal hash
    _(parsed["integer"]).must_be_instance_of Integer
    _(parsed["float"]).must_be_instance_of Float
    _(parsed["big_float"]).must_be_instance_of Float
  end

  it "round-trips special float values" do
    hash = {"inf" => Float::INFINITY, "neg_inf" => -Float::INFINITY, "nan" => Float::NAN}
    parsed = round_trip(hash)
    _(parsed["inf"]).must_equal Float::INFINITY
    _(parsed["neg_inf"]).must_equal(-Float::INFINITY)
    _(parsed["nan"].nan?).must_equal true
  end

  it "round-trips dates and times" do
    hash = {
      "odt" => Time.new(1979, 5, 27, 0, 32, 0.999999, '-07:00'),
      "utc" => Time.utc(1979, 5, 27, 7, 32, 0),
      "ldt" => Tomlrb::LocalDateTime.new(1979, 5, 27, 0, 32, 0.5),
      "ld" => Tomlrb::LocalDate.new(1979, 5, 27),
      "lt" => Tomlrb::LocalTime.new(7, 32, 0.25)
    }
    _(round_trip(hash)).must_equal hash
  end

  it "round-trips nested tables" do
    hash = {
      "a" => {"b" => {"c" => {"w" => "deep"}}},
      "servers" => {
        "alpha" => {"ip" => "10.0.0.1", "dc" => "eqdc10"},
        "beta" => {"ip" => "10.0.0.2", "dc" => "eqdc10"}
      }
    }
    _(round_trip(hash)).must_equal hash
  end

  it "preserves empty tables and empty arrays" do
    hash = {
      "empty_table" => {},
      "nested" => {"also_empty" => {}, "value" => 1},
      "empty_array" => [],
      "empty_table_array" => [{}]
    }
    parsed = round_trip(hash)
    _(parsed).must_equal hash
    _(parsed["empty_table"]).must_equal({})
    _(parsed["nested"]["also_empty"]).must_equal({})
    _(parsed["empty_array"]).must_equal([])
    _(parsed["empty_table_array"]).must_equal([{}])
  end

  it "round-trips mixed arrays" do
    hash = {
      "mixed" => [1, "two", 3.0, true, [1, 2], {"inline" => "table"}],
      "contributors" => ["gamma", "delta"],
      "data" => [["gamma", "delta"], [1, 2]]
    }
    _(round_trip(hash)).must_equal hash
  end

  it "round-trips multi-level arrays of tables" do
    hash = {
      "fruit" => [
        {
          "name" => "apple",
          "physical" => {"color" => "red", "shape" => "round"},
          "variety" => [{"name" => "red delicious"}, {"name" => "granny smith"}]
        },
        {
          "name" => "banana",
          "physical" => {"shape" => "long"},
          "variety" => [{"name" => "plantain"}]
        }
      ]
    }
    _(round_trip(hash)).must_equal hash
  end

  it "keeps values in the correct array of tables instance" do
    hash = {
      "products" => [
        {"name" => "Hammer", "sku" => 738594937},
        {},
        {"name" => "Nail", "sku" => 284758393, "color" => "gray"}
      ]
    }
    _(round_trip(hash)).must_equal hash
  end

  it "treats keys with dots, quotes or whitespace as literal keys" do
    hash = {
      "a.b.c" => "dotted",
      "with space" => "spaced",
      "quote\"d" => "quoted",
      "127.0.0.1" => "ip-like",
      "nested" => {"x.y" => {"z z" => 1}}
    }
    parsed = round_trip(hash)
    _(parsed).must_equal hash
    _(parsed["a.b.c"]).must_equal "dotted"
    _(parsed["nested"]["x.y"]["z z"]).must_equal 1
  end

  it "accepts symbol keys" do
    _(Tomlrb.parse(Tomlrb.dump({title: "sym"}))).must_equal({"title" => "sym"})
  end

  it "raises a DumpError with the path for nil values" do
    error = _{ Tomlrb.dump({"a" => {"b" => nil}}) }.must_raise(Tomlrb::DumpError)
    _(error.message).must_include "a.b"
  end

  it "raises a DumpError with the path for nil values inside arrays" do
    error = _{ Tomlrb.dump({"list" => [1, nil]}) }.must_raise(Tomlrb::DumpError)
    _(error.message).must_include "list[1]"
  end

  it "raises a DumpError with the path for circular references" do
    hash = {"a" => {}}
    hash["a"]["self"] = hash
    error = _{ Tomlrb.dump(hash) }.must_raise(Tomlrb::DumpError)
    _(error.message).must_include "a.self"
  end

  it "raises a DumpError with the path for unsupported values" do
    error = _{ Tomlrb.dump({"a" => {"b" => Object.new}}) }.must_raise(Tomlrb::DumpError)
    _(error.message).must_include "a.b"
    _{ Tomlrb.dump({"sym" => :value}) }.must_raise(Tomlrb::DumpError)
  end

  it "raises a DumpError when the root is not a hash" do
    _{ Tomlrb.dump([1, 2]) }.must_raise(Tomlrb::DumpError)
  end
end
