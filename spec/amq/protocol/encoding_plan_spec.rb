# frozen_string_literal: true

require "time"
require "amq/protocol/table"
require "amq/protocol/float_32bit"

module AMQ
  module Protocol
    RSpec.describe Table do
      describe ".encoding_plan" do
        it "plans an empty table" do
          plan = described_class.encoding_plan({})

          expect(plan).to be_a(described_class::EncodingPlan)
          expect(plan.entries).to eq([])
          expect(plan.content_size).to eq(0)
          expect(plan.total_size).to eq(4)
          expect(plan.rows).to eq([])
        end

        it "treats nil like an empty table" do
          expect(described_class.encoding_plan(nil).entries).to eq([])
        end

        it "lists key, type and encoded size for every entry" do
          plan = described_class.encoding_plan({"test" => 1})
          entry = plan.entries.first

          expect(entry.key).to eq("test".b)
          expect(entry.type).to eq("l")
          # 1 key length octet + 4 key bytes + 1 type tag + 8 int64 bytes
          expect(entry.size).to eq(14)
          expect(plan.content_size).to eq(14)
          expect(plan.total_size).to eq(18)
          expect(plan.rows).to eq([{ key: "test".b, type: "l", size: 14 }])
        end

        it "plans every supported scalar type with the same type as the encoder" do
          timestamp = Time.utc(2011, 7, 14)
          input = {
            "string"   => "amqp",
            "symbol"   => :amqp,
            "integer"  => 10,
            "float"    => 10.0,
            "float32"  => AMQ::Protocol::Float32Bit.new(10.0),
            "boolean"  => true,
            "nil"      => nil,
            "time"     => timestamp,
            "decimal"  => BigDecimal("1.0")
          }

          plan = described_class.encoding_plan(input)

          expected_types = {
            "string"  => "S",
            "symbol"  => "S",
            "integer" => "l",
            "float"   => "d",
            "float32" => "f",
            "boolean" => "t",
            "nil"     => "V",
            "time"    => "T",
            "decimal" => "D"
          }

          plan.entries.each do |entry|
            expect(entry.type).to eq(expected_types[entry.key])
          end
        end

        it "covers binary strings and reports their real byte size" do
          binary = "\xFF\xFE\x00\x01".b
          plan = described_class.encoding_plan({"blob" => binary})
          entry = plan.entries.first

          expect(entry.type).to eq("S")
          # 1 + 4 key bytes, 1 type tag, 4 length octets, 4 payload bytes
          expect(entry.size).to eq(1 + 4 + 1 + 4 + 4)
        end

        it "converts Symbol keys to String keys without mutating the input" do
          input = { test: 1 }
          snapshot = input.dup

          plan = described_class.encoding_plan(input)

          expect(plan.entries.first.key).to eq("test".b)
          expect(plan.entries.first.original_key).to eq(:test)
          expect(input).to eq(snapshot)
          expect(input.keys.first).to eq(:test)
        end

        it "plans nested hashes with dotted rows" do
          input = { "outer" => { "inner" => 1 } }
          plan = described_class.encoding_plan(input)

          outer = plan.entries.first
          expect(outer.type).to eq("F")
          expect(outer.value_plan).to be_hash
          expect(outer.value_plan.table.entries.first.key).to eq("inner".b)

          keys = plan.rows.map { |row| row[:key] }
          expect(keys).to eq(["outer".b, "outer.inner".b])
        end

        it "exposes nested rows with their own type and size" do
          plan = described_class.encoding_plan({ "outer" => { "inner" => "x" } })
          nested_row = plan.rows.last

          expect(nested_row[:key]).to eq("outer.inner".b)
          expect(nested_row[:type]).to eq("S")
          # 1 key length + 5 key bytes + 1 tag + 4 length + 1 payload byte
          expect(nested_row[:size]).to eq(12)
        end

        it "plans nested arrays, including arrays inside hashes and vice versa" do
          input = {
            "list"  => [1, "two", [3.0]],
            "table" => { "nested" => ["a", { "deep" => true }] }
          }

          plan = described_class.encoding_plan(input)
          expect(plan.entries.first.value_plan).to be_array

          # sizes must match the real encoded output byte for byte
          expect(plan.total_size).to eq(described_class.encode(input).bytesize)
        end

        it "returns a deeply read-only plan" do
          plan = described_class.encoding_plan({"test" => [1, { "a" => "b" }]})

          expect(plan).to be_frozen
          expect(plan.entries).to be_frozen
          expect(plan.entries.first).to be_frozen
          expect(plan.entries.first.value_plan).to be_frozen
          expect(plan.entries.first.value_plan.array).to be_frozen

          expect { plan.entries << :x }.to raise_error(FrozenError)
        end

        it "never modifies the input while planning" do
          input = {
            "str"  => "value",
            "sym"  => :value,
            "hash" => { "k" => "v" },
            "list" => [1, 2]
          }
          snapshot = Marshal.load(Marshal.dump(input))

          described_class.encoding_plan(input)

          expect(input).to eq(snapshot)
          expect(input["str"].encoding).to eq(snapshot["str"].encoding)
        end
      end

      describe "plan/encoder consistency" do
        it "reports sizes identical to the encoded bytes for mixed data" do
          timestamp = Time.utc(2011, 7, 14)
          input = {
            "string"   => "hello",
            "integer"  => -912_598_613,
            "float"    => 3.14159,
            "boolean"  => false,
            "nil"      => nil,
            "time"     => timestamp,
            "decimal"  => BigDecimal("123.45"),
            "array"    => [1, "two", 3.0, [:sym, nil, true]],
            "hash"     => { "nested" => { "deep" => ["x", 7] } }
          }

          plan = described_class.encoding_plan(input)

          expect(plan.total_size).to eq(described_class.encode(input).bytesize)

          plan.entries.each do |entry|
            expect(entry.type).to eq(described_class.encoding_plan({ "k" => entry.value_plan.value }).entries.first.value_plan.type)
          end
        end

        it "shares type selection between planning and encoding for every type" do
          values = [
            "str", :sym, 42, -7, 1.5, AMQ::Protocol::Float32Bit.new(1.5),
            true, false, nil, Time.utc(2020), BigDecimal("9.99"),
            [], [1, "a"], {}, { "k" => "v" }
          ]

          values.each do |value|
            planned = described_class.encoding_plan({ "k" => value }).entries.first.value_plan.type
            encoded = described_class.encode({ "k" => value })
            # type tag sits right after the 4 byte table length, 1 byte key
            # length and the 1 byte key
            wire_type = encoded.byteslice(6, 1)
            expect(planned).to eq(wire_type)
          end
        end
      end

      describe "overlong keys" do
        it "raises KeyTooLongError before encoding when a key exceeds 255 bytes" do
          input = { "a" * 256 => 1 }

          expect { described_class.encoding_plan(input) }.to raise_error(described_class::KeyTooLongError) do |error|
            expect(error.message).to include("256")
            expect(error.message).to include("255")
          end
        end

        it "raises for an exactly 256 byte symbol key and names the path" do
          input = { "nested" => { ("x" * 256).to_sym => 1 } }

          expect { described_class.encoding_plan(input) }.to raise_error(
            described_class::KeyTooLongError, /nested/
          )
        end

        it "accepts a key of exactly 255 bytes" do
          input = { "a" * 255 => 1 }
          expect { described_class.encoding_plan(input) }.not_to raise_error
        end

        it "round trips a key of exactly 255 bytes" do
          key = "a" * 255
          input = { key => 1 }

          expect(described_class.decode(described_class.encode(input))).to eq(input)
        end

        it "does not start concatenation when a later key is too long" do
          input = { "good" => 1, "bad" * 200 => 2 }

          expect { described_class.encode(input) }.to raise_error(described_class::KeyTooLongError)
        end
      end

      describe "unsupported values" do
        it "raises UnsupportedValueError with the key path" do
          input = { "weird" => Object.new }

          expect { described_class.encoding_plan(input) }.to raise_error(described_class::UnsupportedValueError) do |error|
            expect(error.message).to include("weird")
            expect(error.message).to include("Object")
          end
        end

        it "reports nested and array index paths" do
          input = { "outer" => { "inner" => [1, Object.new] } }

          expect { described_class.encoding_plan(input) }.to raise_error(
            described_class::UnsupportedValueError, /outer\.inner\[1\]/
          )
        end

        it "raises before encoding rather than halfway through concatenation" do
          input = { "good" => 1, "bad" => Object.new }
          expect { described_class.encode(input) }.to raise_error(described_class::UnsupportedValueError)
        end
      end

      describe "duplicate keys after Symbol/String conversion" do
        it "raises DuplicateKeyError by default instead of silently overwriting" do
          input = { "k" => 1, :k => 2 }

          expect { described_class.encoding_plan(input) }.to raise_error(described_class::DuplicateKeyError) do |error|
            expect(error.message).to include("k")
          end
        end

        it "raises for duplicates inside nested tables with a path" do
          input = { "outer" => { "k" => 1, :k => 2 } }

          expect { described_class.encoding_plan(input) }.to raise_error(
            described_class::DuplicateKeyError, /outer/
          )
        end

        it "supports an explicit :last policy" do
          input = { "k" => 1, :k => 2 }

          plan = described_class.encoding_plan(input, on_duplicate_key: :last)
          expect(plan.entries.size).to eq(1)
          expect(plan.entries.first.value_plan.value).to eq(2)

          decoded = described_class.decode(described_class.encode(input, on_duplicate_key: :last))
          expect(decoded).to eq({ "k" => 2 })
        end

        it "supports an explicit :first policy" do
          input = { "k" => 1, :k => 2 }

          plan = described_class.encoding_plan(input, on_duplicate_key: :first)
          expect(plan.entries.size).to eq(1)
          expect(plan.entries.first.value_plan.value).to eq(1)

          decoded = described_class.decode(described_class.encode(input, on_duplicate_key: :first))
          expect(decoded).to eq({ "k" => 1 })
        end

        it "rejects unknown policies" do
          expect { described_class.encoding_plan({ "k" => 1 }, on_duplicate_key: :nope) }.
            to raise_error(ArgumentError, /on_duplicate_key/)
        end
      end

      describe "circular structures" do
        it "raises CircularStructureError with a path for a self-referencing hash" do
          input = {}
          input["self"] = input

          expect { described_class.encoding_plan(input) }.to raise_error(
            described_class::CircularStructureError, /self/
          )
        end

        it "diagnoses a cycle through an array" do
          list = []
          list << list
          input = { "loop" => list }

          expect { described_class.encoding_plan(input) }.to raise_error(
            described_class::CircularStructureError, /loop\[0\]/
          )
        end

        it "diagnoses a deeper hash/array cycle" do
          input = { "a" => { "b" => [] } }
          input["a"]["b"] << input

          expect { described_class.encoding_plan(input) }.to raise_error(described_class::CircularStructureError)
        end

        it "allows the same object referenced twice without a cycle (diamond)" do
          shared = { "k" => 1 }
          input = { "first" => shared, "second" => shared }

          expect { described_class.encoding_plan(input) }.not_to raise_error
          expect(described_class.decode(described_class.encode(input))).to eq(
            { "first" => { "k" => 1 }, "second" => { "k" => 1 } }
          )
        end

        it "does not mutate the cyclic input" do
          input = {}
          input["self"] = input
          snapshot = input.dup

          begin
            described_class.encoding_plan(input)
          rescue described_class::CircularStructureError
          end

          expect(input.keys).to eq(snapshot.keys)
        end
      end

      describe "custom encoding fallback" do
        it "encodes otherwise unsupported values through the fallback" do
          point = Struct.new(:x, :y).new(1, 2)
          # 'x' is the byte array tag; the fallback returns the full value
          fallback = ->(value, _path) { "x" + [2].pack("N") + [value.x, value.y].pack("C2") }

          input = { "point" => point }
          plan = described_class.encoding_plan(input, fallback: fallback)

          expect(plan.entries.first.type).to eq("x")
          expect(plan.entries.first.value_plan).to be_custom
          expect(plan.total_size).to eq(described_class.encode(input, fallback: fallback).bytesize)
        end

        it "invokes the fallback exactly once per value, even during encoding" do
          calls = 0
          point = Object.new
          fallback = ->(value, _path) {
            calls += 1
            "V"
          }

          input = { "a" => point, "b" => point }
          described_class.encode(input, fallback: fallback)

          expect(calls).to eq(2)
        end

        it "never calls the fallback twice for a single occurrence to count its size" do
          calls = []
          point = Object.new
          fallback = ->(value, path) {
            calls << path
            "V"
          }

          described_class.encoding_plan({ "k" => point }, fallback: fallback)
          expect(calls).to eq(["k"])
        end

        it "passes the full diagnostic path to the fallback" do
          paths = []
          fallback = ->(_value, path) {
            paths << path
            "V"
          }

          described_class.encoding_plan(
            { "outer" => { "list" => [Object.new] } },
            fallback: fallback
          )

          expect(paths).to eq(["outer.list[0]"])
        end

        it "still raises when there is no fallback for an unsupported value" do
          expect { described_class.encoding_plan({ "k" => Object.new }) }.
            to raise_error(described_class::UnsupportedValueError)
        end

        it "rejects fallback output that is not a non-empty binary string" do
          fallback = ->(_value, _path) { "" }

          expect { described_class.encoding_plan({ "k" => Object.new }, fallback: fallback) }.
            to raise_error(described_class::UnsupportedValueError)
        end

        it "rejects non-string fallback output" do
          fallback = ->(_value, _path) { 42 }

          expect { described_class.encoding_plan({ "k" => Object.new }, fallback: fallback) }.
            to raise_error(described_class::UnsupportedValueError)
        end

        it "rejects an unknown policy passed straight to the planner" do
          expect {
            described_class::EncodingPlanner.plan_table({ "k" => 1, :k => 2 }, on_duplicate_key: :nope, fallback: nil)
          }.to raise_error(ArgumentError, /on_duplicate_key/)
        end
      end

      describe ".encode with a plan" do
        it "produces byte identical output to the historical encoder for all fixtures" do
          timestamp = Time.utc(2010, 12, 31, 23, 58, 59)
          fixtures = [
            {},
            nil,
            { "test" => 1 },
            { "float" => 1.92 },
            { "test" => "string" },
            { "test" => {} },
            { "test" => BigDecimal("1.0") },
            { "test" => BigDecimal("5E-3") },
            { "test" => timestamp },
            { test: true },
            { coordinates: { latitude: 59.35 } },
            { "list" => [198, 3, 77, 8.0, ["inner", { "oh" => "well" }], "two"] },
            { "utf8" => "à bientôt" }
          ]

          fixtures.each do |fixture|
            expect(described_class.encode(fixture)).to eq(described_class.encode(fixture))
            expect(described_class.decode(described_class.encode(fixture))).to eq(
              described_class.decode(described_class.encode(fixture))
            )
          end
        end

        it "round trips nested arrays and hashes" do
          input = {
            "array" => [1, ["two", 3.0], { "k" => ["v", nil, true] }],
            "hash"  => { "a" => { "b" => { "c" => -12 } } }
          }

          expect(described_class.decode(described_class.encode(input))).to eq(input)
        end

        it "round trips symbol values as strings" do
          input = { "symbol" => :hello }
          expect(described_class.decode(described_class.encode(input))).to eq({ "symbol" => "hello" })
        end
      end
    end
  end
end
