require 'time'
require "amq/protocol/table_value_encoder"
require "amq/protocol/float_32bit"

module AMQ
  module Protocol
    RSpec.describe TableValueEncoder do

      it "calculates size of string field values" do
        expect(described_class.field_value_size("amqp")).to eq(9)
        expect(described_class.encode("amqp").bytesize).to eq(9)

        expect(described_class.field_value_size("amq-protocol")).to eq(17)
        expect(described_class.encode("amq-protocol").bytesize).to eq(17)

        expect(described_class.field_value_size("à bientôt")).to eq(16)
        expect(described_class.encode("à bientôt").bytesize).to eq(16)
      end

      it "calculates size of integer field values" do
        expect(described_class.field_value_size(10)).to eq(9)
        expect(described_class.encode(10).bytesize).to eq(9)
      end

      it "calculates size of float field values (considering them to be 64-bit)" do
        expect(described_class.field_value_size(10.0)).to eq(9)
        expect(described_class.encode(10.0).bytesize).to eq(9)

        expect(described_class.field_value_size(120000.0)).to eq(9)
        expect(described_class.encode(120000.0).bytesize).to eq(9)
      end

      it "calculates size of float field values (boxed as 32-bit)" do
        expect(described_class.encode(AMQ::Protocol::Float32Bit.new(10.0)).bytesize).to eq(5)
        expect(described_class.encode(AMQ::Protocol::Float32Bit.new(120000.0)).bytesize).to eq(5)
      end

      it "calculates size of boolean field values" do
        expect(described_class.field_value_size(true)).to eq(2)
        expect(described_class.encode(true).bytesize).to eq(2)

        expect(described_class.field_value_size(false)).to eq(2)
        expect(described_class.encode(false).bytesize).to eq(2)
      end

      it "calculates size of void field values" do
        expect(described_class.field_value_size(nil)).to eq(1)
        expect(described_class.encode(nil).bytesize).to eq(1)
      end

      it "calculates size of time field values" do
        t = Time.parse("2011-07-14 01:17:46 +0400")

        expect(described_class.field_value_size(t)).to eq(9)
        expect(described_class.encode(t).bytesize).to eq(9)
      end


      it "calculates size of basic table field values" do
        input1   = { "key" => "value" }
        expect(described_class.field_value_size(input1)).to eq(19)
        expect(described_class.encode(input1).bytesize).to eq(19)


        input2   = { "intval" => 1 }
        expect(described_class.field_value_size(input2)).to eq(21)
        expect(described_class.encode(input2).bytesize).to eq(21)


        input3   = { "intval" => 1, "key" => "value" }
        expect(described_class.field_value_size(input3)).to eq(35)
        expect(described_class.encode(input3).bytesize).to eq(35)
      end


      it "calculates size of table field values" do
        input1   = {
          "hashval"    => {
            "protocol" => {
              "name"  => "AMQP",
              "major" => 0,
              "minor" => "9",
              "rev"   => 1.0,
              "spec"  => {
                "url"  => "http://bit.ly/hw2ELX",
                "utf8" => "à bientôt".force_encoding(::Encoding::ASCII_8BIT)
              }
            },
            "true"     => true,
            "false"    => false,
            "nil"      => nil
          }
        }

        expect(described_class.field_value_size(input1)).to eq(166)
        # puts(described_class.encode(input1).inspect)
        expect(described_class.encode(input1).bytesize).to eq(166)



        input2   = {
          "boolval"      => true,
          "intval"       => 1,
          "strval"       => "Test",
          "timestampval" => Time.parse("2011-07-14 01:17:46 +0400"),
          "floatval"     => 3.14,
          "longval"      => 912598613,
          "hashval"      => { "protocol" => "AMQP091", "true" => true, "false" => false, "nil" => nil }
        }

        expect(described_class.field_value_size(input2)).to eq(158)
        expect(described_class.encode(input2).bytesize).to eq(158)
      end

      it "calculates size of basic array field values" do
        input1 = [1, 2, 3]

        expect(described_class.field_value_size(input1)).to eq(32)
        expect(described_class.encode(input1).bytesize).to eq(32)


        input2 = ["one", "two", "three"]
        expect(described_class.field_value_size(input2)).to eq(31)
        expect(described_class.encode(input2).bytesize).to eq(31)


        input3 = ["one", 2, "three"]
        expect(described_class.field_value_size(input3)).to eq(32)
        expect(described_class.encode(input3).bytesize).to eq(32)


        input4 = ["one", 2, "three", ["four", 5, [6.0]]]
        expect(described_class.field_value_size(input4)).to eq(69)
        expect(described_class.encode(input4).bytesize).to eq(69)
      end

      it "encodes symbols as strings" do
        encoded = described_class.encode(:test_symbol)
        expect(encoded).to start_with("S")
        expect(encoded).to include("test_symbol")
      end

      it "encodes BigDecimal values" do
        bd = BigDecimal("123.45")
        encoded = described_class.encode(bd)
        expect(encoded).to start_with("D")
      end

      it "encodes BigDecimal with zero exponent" do
        bd = BigDecimal("100")
        encoded = described_class.encode(bd)
        expect(encoded).to start_with("D")
      end

      it "calculates size of BigDecimal field values consistently with encoding" do
        bd = BigDecimal("123.45")
        expect(described_class.field_value_size(bd)).to eq(6)
        expect(described_class.encode(bd).bytesize).to eq(6)
      end

      describe "BigDecimal encoding modes" do
        after(:each) do
          described_class.big_decimal_mode = :legacy
        end

        it "defaults to the legacy mode for backwards compatibility" do
          expect(described_class.big_decimal_mode).to eq(:legacy)
        end

        it "rejects unknown modes" do
          expect { described_class.big_decimal_mode = :bogus }.to raise_error(ArgumentError, /unknown BigDecimal encoding mode/)
        end

        context "in legacy mode" do
          it "keeps the historical wire representation" do
            expect(described_class.encode(BigDecimal("1.0"))).to eq("D\x00\x00\x00\x00\x01".b)
            expect(described_class.encode(BigDecimal("5E-3"))).to eq("D\x03\x00\x00\x00\x05".b)
          end
        end

        context "in exact mode" do
          before(:each) do
            described_class.big_decimal_mode = :exact
          end

          it "encodes scale and integer coefficient per the protocol" do
            expect(described_class.encode(BigDecimal("123.45"))).to eq("D".b + "\x02".b + [12345].pack("l>"))
          end

          it "encodes negative values with a signed coefficient" do
            expect(described_class.encode(BigDecimal("-123.45"))).to eq("D".b + "\x02".b + [-12345].pack("l>"))
          end

          it "encodes values in scientific notation" do
            expect(described_class.encode(BigDecimal("5E-3"))).to eq("D".b + "\x03".b + [5].pack("l>"))
            expect(described_class.encode(BigDecimal("1E+3"))).to eq("D".b + "\x00".b + [1000].pack("l>"))
          end

          it "encodes zero canonically" do
            expect(described_class.encode(BigDecimal("0"))).to eq("D".b + "\x00".b + [0].pack("l>"))
            expect(described_class.encode(BigDecimal("-0"))).to eq("D".b + "\x00".b + [0].pack("l>"))
          end

          it "accepts coefficient boundary values" do
            expect(described_class.encode(BigDecimal("2147483647"))).to eq("D".b + "\x00".b + [2**31 - 1].pack("l>"))
            expect(described_class.encode(BigDecimal("-2147483648"))).to eq("D".b + "\x00".b + [-2**31].pack("l>"))
          end

          it "rejects coefficients outside the signed 32-bit range" do
            expect { described_class.encode(BigDecimal("2147483648")) }.to raise_error(ArgumentError, /coefficient/)
            expect { described_class.encode(BigDecimal("-2147483649")) }.to raise_error(ArgumentError, /coefficient/)
          end

          it "rejects scales that do not fit into an octet" do
            expect { described_class.encode(BigDecimal("1E-256")) }.to raise_error(ArgumentError, /scale/)
          end

          it "accepts the maximum representable scale" do
            encoded = described_class.encode(BigDecimal("1E-255"))
            expect(encoded.bytesize).to eq(6)
            expect(encoded.getbyte(1)).to eq(255)
          end

          it "rejects non-finite values instead of emitting a payload" do
            expect { described_class.encode(BigDecimal("Infinity")) }.to raise_error(ArgumentError, /non-finite/)
            expect { described_class.encode(BigDecimal("-Infinity")) }.to raise_error(ArgumentError, /non-finite/)
            expect { described_class.encode(BigDecimal("NaN")) }.to raise_error(ArgumentError, /non-finite/)
          end

          it "round-trips numeric values (not their textual form)" do
            ["123.45", "-123.45", "0", "5E-3", "1E+3", "100", "0.1", "1.00"].each do |s|
              decoded, _ = TableValueDecoder.decode_big_decimal(described_class.encode(BigDecimal(s))[1..-1], 0)
              expect(decoded).to eq(BigDecimal(s))
            end
          end

          it "keeps field_value_size consistent with the encoded size" do
            bd = BigDecimal("123.45")
            expect(described_class.field_value_size(bd)).to eq(described_class.encode(bd).bytesize)
          end

          it "applies the same implementation to nested tables and arrays" do
            input = {
              "price"  => BigDecimal("-9.99"),
              "nested" => { "d" => BigDecimal("5E-3") },
              "arr"    => [BigDecimal("1.5"), BigDecimal("-2.25")]
            }
            expect(Table.decode(Table.encode(input))).to eq(input)
          end

          it "rejects non-finite values nested in tables and arrays" do
            expect { Table.encode({ "bad" => BigDecimal("NaN") }) }.to raise_error(ArgumentError, /non-finite/)
            expect { Table.encode({ "bad" => [BigDecimal("Infinity")] }) }.to raise_error(ArgumentError, /non-finite/)
          end
        end
      end


      it "raises ArgumentError for unsupported types" do
        expect { described_class.encode(Object.new) }.to raise_error(ArgumentError)
      end

    end # TableValueEncoder
  end # Protocol
end # AMQ
