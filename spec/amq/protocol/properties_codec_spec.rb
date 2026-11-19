# encoding: binary

module AMQ
  module Protocol
    RSpec.describe AMQ::Protocol::PropertiesCodec do
      let(:basic_descriptions) { AMQ::Protocol::Basic::PROPERTY_DESCRIPTIONS }

      # 17 in-memory properties: more than the 15 a single flags word can
      # announce, so encoding/decoding has to use a continuation word.
      let(:extended_descriptions) do
        (0..14).map { |i| [:"octet_#{i}", :octet] } +
          [[:title, :shortstr], [:attributes, :table]]
      end

      describe '.encode' do
        it 'encodes a single flags word with only the declared properties' do
          result = PropertiesCodec.encode(basic_descriptions, {:priority => 3, :content_type => 'text/plain'})
          expect(result).to eq("\x88\x00\ntext/plain\x03")
        end

        it 'encodes an empty flags word when no properties are given' do
          expect(PropertiesCodec.encode(basic_descriptions, {})).to eq("\x00\x00")
        end

        it 'skips properties with nil values' do
          result = PropertiesCodec.encode(basic_descriptions, {:priority => 3, :content_type => nil})
          expect(result).to eq("\x08\x00\x03")
        end

        it 'raises UnknownProperty for properties missing from the descriptions' do
          expect {
            PropertiesCodec.encode(basic_descriptions, {:not_a_property => 1})
          }.to raise_error(AMQ::Protocol::UnknownProperty, /not_a_property/)
        end

        it 'sets the continuation bit and emits a second flags word for extended layouts' do
          result = PropertiesCodec.encode(extended_descriptions, {:octet_0 => 1, :octet_14 => 2, :title => 'hello'})
          expect(result).to eq("\x80\x03\x80\x00\x01\x02\x05hello")
        end

        it 'round-trips extended layouts through .decode' do
          properties = {:octet_0 => 1, :octet_7 => 8, :title => 'hello', :attributes => {'a' => 'b'}}
          encoded = PropertiesCodec.encode(extended_descriptions, properties)
          expect(PropertiesCodec.decode(extended_descriptions, encoded)).to eq(properties)
        end
      end

      describe '.decode' do
        it 'decodes only the properties announced by the flags word' do
          result = PropertiesCodec.decode(basic_descriptions, "\x98\x40\x18application/octet-stream\x02\x00\x00\x00\x00\x00\x00\x00\x00{")
          expect(result).to eq({:priority => 0, :delivery_mode => 2, :content_type => 'application/octet-stream', :timestamp => Time.at(123)})
        end

        it 'reads a second flags word when the continuation bit is set' do
          result = PropertiesCodec.decode(extended_descriptions, "\x80\x03\x80\x00\x01\x02\x05hello")
          expect(result).to eq({:octet_0 => 1, :octet_14 => 2, :title => 'hello'})
        end

        it 'raises MissingPropertyFlagsContinuation when the continuation bit is set but the frame ends' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x00\x01")
          }.to raise_error(AMQ::Protocol::MissingPropertyFlagsContinuation, /flags word 1/)
        end

        it 'raises MissingPropertyFlagsContinuation when the frame has no flags word at all' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "")
          }.to raise_error(AMQ::Protocol::MissingPropertyFlagsContinuation, /flags word 0/)
        end

        it 'raises UnknownPropertyFlags for flag bits without a declared property' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x00\x02\x01")
          }.to raise_error(AMQ::Protocol::UnknownPropertyFlags, /0x0002 in flags word 0/)
        end

        it 'raises UnknownPropertyFlags for undeclared bits in a continuation word' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x00\x01\x00\x02")
          }.to raise_error(AMQ::Protocol::UnknownPropertyFlags, /0x0002 in flags word 1/)
        end

        it 'raises TruncatedPropertyValue for a truncated shortstr without reading past the frame' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x80\x00\x18application")
          }.to raise_error(AMQ::Protocol::TruncatedPropertyValue, /content_type/)
        end

        it 'raises TruncatedPropertyValue for a truncated octet' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x10\x00")
          }.to raise_error(AMQ::Protocol::TruncatedPropertyValue, /delivery_mode/)
        end

        it 'raises TruncatedPropertyValue for a truncated timestamp' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x00\x40\x00\x00\x00")
          }.to raise_error(AMQ::Protocol::TruncatedPropertyValue, /timestamp/)
        end

        it 'raises TruncatedPropertyValue for a truncated table' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x20\x00\x00\x00\x00\x10\x05helloS\x00")
          }.to raise_error(AMQ::Protocol::TruncatedPropertyValue, /headers/)
        end

        it 'raises UnexpectedPropertyBytes when bytes remain after the last property' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x08\x00\x03\xff\xff")
          }.to raise_error(AMQ::Protocol::UnexpectedPropertyBytes, /2 unexpected byte/)
        end

        it 'raises UnexpectedPropertyBytes for a flags-only frame with trailing bytes' do
          expect {
            PropertiesCodec.decode(basic_descriptions, "\x00\x00\xce")
          }.to raise_error(AMQ::Protocol::UnexpectedPropertyBytes, /1 unexpected byte/)
        end
      end
    end
  end
end
