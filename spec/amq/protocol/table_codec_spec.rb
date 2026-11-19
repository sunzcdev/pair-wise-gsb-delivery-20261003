# encoding: binary
# frozen_string_literal: true

require "amq/protocol/table"

module AMQ
  module Protocol
    # A boxed Ruby value only the plugin codec in these examples knows about.
    class PluginValue
      attr_reader :payload

      def initialize(payload)
        @payload = payload
      end

      def ==(other)
        other.is_a?(PluginValue) && other.payload == payload
      end
    end

    RSpec.describe Table::CodecSet do
      # A private type code used only by the plugin codecs in these examples.
      PRIVATE_TYPE = "z".freeze

      let(:codec) do
        Table::Codec.new(
          type: PRIVATE_TYPE,
          match:   ->(value) { value.is_a?(PluginValue) },
          encoder: ->(value) { [value.payload.bytesize].pack("N") << value.payload },
          sizer:   ->(value) { 4 + value.payload.bytesize },
          decoder: ->(data, offset) {
            length = data.byteslice(offset, 4).unpack1("N")
            [PluginValue.new(data.byteslice(offset + 4, length)), offset + 4 + length]
          }
        )
      end

      let(:codecs) { Table::CodecSet.build([codec]) }

      describe Table::Codec do
        it "rejects type codes that are not exactly one byte" do
          expect { Table::Codec.new(type: "ab") }.to raise_error(Table::TableCodecError, /single byte/)
          expect { Table::Codec.new(type: 7) }.to raise_error(Table::TableCodecError, /single byte/)
        end

        it "is frozen once built" do
          expect(codec).to be_frozen
          expect(codec.type).to be_frozen
        end
      end

      describe ".build" do
        it "includes every built-in type code" do
          set = Table::CodecSet.build

          %w[S I T D F A b d f l s t x V].each do |tag|
            expect(set.key?(tag)).to be(true)
          end
        end

        it "freezes the resulting set so it cannot change later" do
          expect(codecs).to be_frozen
        end

        it "rejects a custom codec that collides with a built-in type code" do
          conflicting = Table::Codec.new(type: "S", decoder: ->(d, o) { [nil, o] })

          expect { Table::CodecSet.build([conflicting]) }
            .to raise_error(Table::CodecConflictError, /already registered/)
        end

        it "rejects two custom codecs that claim the same private type code" do
          other = Table::Codec.new(type: PRIVATE_TYPE, decoder: ->(d, o) { [nil, o] })

          expect { Table::CodecSet.build([codec, other]) }
            .to raise_error(Table::CodecConflictError, /already registered/)
        end

        it "rejects objects that are not Codec instances" do
          expect { Table::CodecSet.build([:not_a_codec]) }
            .to raise_error(Table::TableCodecError, /must be an AMQ::Protocol::Table::Codec/)
        end
      end

      describe "session isolation" do
        it "does not change the behavior of a session that uses the default codecs" do
          expect(codecs.key?(PRIVATE_TYPE)).to be(true)
          expect(Table::CodecSet.build.key?(PRIVATE_TYPE)).to be(false)

          # The built-in set is unaffected and still rejects the private type.
          wire = [6].pack("N") + "\x02k=z".b + "\x00\x00\x00\x01x"
          expect { Table.decode(wire) }.to raise_error(Table::UnknownTypeError)
        end

        it "keeps the global type constant table untouched" do
          before = TypeConstants.constants.map { |c| [c, TypeConstants.const_get(c)] }.to_h

          Table::CodecSet.build([codec])

          after = TypeConstants.constants.map { |c| [c, TypeConstants.const_get(c)] }.to_h
          expect(after).to eq(before)
          expect(TypeConstants.constants.map(&:to_s)).not_to include("TYPE_PRIVATE")
        end

        it "leaves no trace of the private type on a freshly built default set" do
          Table::CodecSet.build([codec])

          fresh = Table::CodecSet.build
          expect(fresh.key?(PRIVATE_TYPE)).to be(false)
        end
      end

      describe "encoding and decoding" do
        it "round-trips a plugin value at the top level" do
          table = { "plugin" => PluginValue.new("hello") }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end

        it "writes the private type code for matched values" do
          encoded = Table.encode({ "k" => PluginValue.new("x") }, codecs: codecs)
          payload = encoded.byteslice(4..-1)

          expect(payload).to eq("\x01k".b << PRIVATE_TYPE << [1].pack("N") << "x".b)
        end

        it "falls back to built-in encoding for values no custom codec claims" do
          table = { "n" => 7, "s" => "text", "flag" => true }

          expect(Table.encode(table, codecs: codecs)).to eq(Table.encode(table))
        end

        it "round-trips plugin values nested inside a table" do
          table = { "outer" => { "inner" => PluginValue.new("deep") } }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end

        it "round-trips plugin values nested inside an array" do
          table = { "items" => [1, PluginValue.new("a"), PluginValue.new("b")] }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end

        it "round-trips plugin values inside arrays that are inside tables" do
          table = { "wrap" => [{ "p" => PluginValue.new("mixed") }] }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end
      end

      describe "unknown types" do
        it "stops instead of guessing how many bytes to skip" do
          # key "k", type '?' with trailing bytes that must never be consumed
          wire = [8].pack("N") + "\x01k?".b + "\x00\x00\x00\x01x"

          expect { Table.decode(wire, codecs: codecs) }
            .to raise_error(Table::UnknownTypeError, /cannot determine value length/)
        end

        it "stops on an unknown type inside an array" do
          array = "\x00".b + [6].pack("N") << "?".b << "\x00\x00\x00\x01x"

          expect { TableValueDecoder.decode_array(array, 1, codecs) }
            .to raise_error(Table::UnknownTypeError)
        end
      end

      describe "custom decoder length diagnostics" do
        it "rejects a decoder that reports an offset before the value start" do
          bad = Table::Codec.new(type: PRIVATE_TYPE, decoder: ->(data, offset) { [nil, offset - 1] })
          set = Table::CodecSet.build([bad])
          wire = [6].pack("N") + "\x01k".b << PRIVATE_TYPE << "\x00"

          expect { Table.decode(wire, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /before the start offset/)
        end

        it "rejects a decoder that reports an offset past the enclosing table" do
          bad = Table::Codec.new(type: PRIVATE_TYPE, decoder: ->(data, offset) { [nil, offset + 99] })
          set = Table::CodecSet.build([bad])
          wire = [6].pack("N") + "\x01k".b << PRIVATE_TYPE << "\x00"

          expect { Table.decode(wire, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /past the end/)
        end

        it "rejects a decoder that does not return a value/offset pair" do
          bad = Table::Codec.new(type: PRIVATE_TYPE, decoder: ->(data, offset) { :nope })
          set = Table::CodecSet.build([bad])
          wire = [6].pack("N") + "\x01k".b << PRIVATE_TYPE << "\x00"

          expect { Table.decode(wire, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /must return \[value, new_offset\]/)
        end

        it "rejects a decoder that reports a non-integer offset" do
          bad = Table::Codec.new(type: PRIVATE_TYPE, decoder: ->(data, offset) { [nil, "9"] })
          set = Table::CodecSet.build([bad])
          wire = [6].pack("N") + "\x01k".b << PRIVATE_TYPE << "\x00"

          expect { Table.decode(wire, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /non-integer offset/)
        end
      end

      describe "custom length rules" do
        it "diagnoses a sizer that disagrees with the bytes the encoder produces" do
          dishonest = Table::Codec.new(
            type: PRIVATE_TYPE,
            match:   ->(value) { value.is_a?(PluginValue) },
            encoder: ->(value) { value.payload },
            sizer:   ->(value) { value.payload.bytesize + 10 },
            decoder: codec.decoder
          )
          set = Table::CodecSet.build([dishonest])

          expect { Table.encode({ "k" => PluginValue.new("x") }, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /reported a value length of \d+ bytes but encoded \d+ bytes/)
        end

        it "keeps the encoded table framing consistent with an honest sizer" do
          table = { "k" => PluginValue.new("xy") }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.length(encoded)).to eq(encoded.bytesize - 4)
          expect(Table.hash_size(table, codecs: codecs)).to eq(encoded.bytesize - 4)
        end

        it "measures via the encoder when no sizer is provided" do
          no_sizer = Table::Codec.new(
            type: PRIVATE_TYPE,
            match:   ->(value) { value.is_a?(PluginValue) },
            encoder: ->(value) { [value.payload.bytesize].pack("N") << value.payload },
            decoder: codec.decoder
          )
          set = Table::CodecSet.build([no_sizer])
          table = { "k" => PluginValue.new("xy") }

          encoded = Table.encode(table, codecs: set)
          expect(Table.hash_size(table, codecs: set)).to eq(encoded.bytesize - 4)
        end

        it "rejects a sizer that reports a negative length" do
          bad = Table::Codec.new(
            type: PRIVATE_TYPE,
            match:   ->(value) { value.is_a?(PluginValue) },
            encoder: ->(value) { value.payload },
            sizer:   ->(_value) { -1 },
            decoder: codec.decoder
          )
          set = Table::CodecSet.build([bad])

          expect { Table.encode({ "k" => PluginValue.new("x") }, codecs: set) }
            .to raise_error(Table::InvalidOffsetError, /non-negative integer/)
        end
      end

      describe "decode-only codecs" do
        it "supports a private type that can be read but is never matched for encoding" do
          raw_value = "\x00\x00\x00\x03abc".b
          decode_only = Table::Codec.new(
            type: PRIVATE_TYPE,
            decoder: ->(data, offset) {
              length = data.byteslice(offset, 4).unpack1("N")
              [data.byteslice(offset + 4, length), offset + 4 + length]
            }
          )
          set = Table::CodecSet.build([decode_only])

          wire = [9].pack("N") + "\x01k".b << PRIVATE_TYPE << raw_value
          expect(Table.decode(wire, codecs: set)).to eq({ "k" => "abc" })
          expect(set.custom_value?("anything")).to be(false)
        end
      end

      describe "nested container boundaries" do
        it "accepts a custom value that ends exactly at the array boundary" do
          table = { "items" => [PluginValue.new("end")] }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end

        it "accepts a custom value that ends exactly at a nested table boundary" do
          table = { "outer" => { "inner" => PluginValue.new("edge") } }

          encoded = Table.encode(table, codecs: codecs)
          expect(Table.decode(encoded, codecs: codecs)).to eq(table)
        end
      end
    end
  end
end
