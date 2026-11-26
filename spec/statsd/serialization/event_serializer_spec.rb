require 'spec_helper'

describe Datadog::Statsd::Serialization::EventSerializer do
  subject do
    described_class.new(container_id, external_data, global_tags: global_tags, max_payload_size: max_payload_size)
  end

  let(:external_data) do
    nil
  end

  let(:container_id) do
    nil
  end

  let(:global_tags) do
    double('global tags')
  end

  let(:max_payload_size) do
    Datadog::Statsd::MAX_EVENT_SIZE
  end

  describe '#format' do
    before do
      allow(Datadog::Statsd::Serialization::TagSerializer)
        .to receive(:new)
        .with(global_tags)
        .and_return(tag_serializer)
    end

    let(:tag_serializer) do
      instance_double(Datadog::Statsd::Serialization::TagSerializer,
        format: nil
      )
    end

    it 'serializes the event correctly' do
      expect(subject.format('this is a title', 'this is a longer text'))
        .to eq '_e{15,21}:this is a title|this is a longer text'
    end

    it 'serializes events containing non-ascii characters correctly' do
      # the _e{..,..} syntax measures in bytes, not characters
      # 🎈 is f09f8e88 in utf-8
      # 🤡 is f09fa4a1 in utf-8
      expect(subject.format('🎈', '🤡'))
        .to eq '_e{4,4}:🎈|🤡'
    end

    context 'when there are global tags' do
      before do
        allow(tag_serializer)
          .to receive(:format)
          .with(nil)
          .and_return('a-global-tag,another-global')
      end

      it 'serializes the event correctly with global tags' do
        expect(subject.format('this is a title', 'this is a longer text'))
          .to eq '_e{15,21}:this is a title|this is a longer text|#a-global-tag,another-global'
      end
    end

    context 'when the event description is too long (> 8KB)' do
      it 'raises a specific error' do
        expect do
          subject.format('this is a title', 'this is a longer text' * 1000)
        end.to raise_error(RuntimeError, /payload is too big/)
      end

      context 'when the truncate_if_too_long option is true' do
        let(:options) { { truncate_if_too_long: true } }

        it 'truncates the overage' do
          expect(subject.format('this is a title', 'this is a longer text' * 1000, options).bytesize)
            .to eq Datadog::Statsd::MAX_EVENT_SIZE
        end
      end
    end

    context 'when having an alert type' do
      it 'serializes the event correctly with a known alert type' do
        expect(subject.format('this is a title', 'this is a longer text', alert_type: 'warning'))
          .to eq '_e{15,21}:this is a title|this is a longer text|t:warning'
      end

      it 'serializes the event correctly with an unknown alert type' do
        expect(subject.format('this is a title', 'this is a longer text', alert_type: 'yolo'))
          .to eq '_e{15,21}:this is a title|this is a longer text|t:yolo'
      end
    end

    context 'when having a priority' do
      it 'serializes the event correctly with a known alert type' do
        expect(subject.format('this is a title', 'this is a longer text', priority: 'low'))
          .to eq '_e{15,21}:this is a title|this is a longer text|p:low'
      end

      it 'serializes the event correctly with an unknown alert type' do
        expect(subject.format('this is a title', 'this is a longer text', priority: 'urgente'))
          .to eq '_e{15,21}:this is a title|this is a longer text|p:urgente'
      end
    end

    context 'when having a timestamp' do
      it 'serializes the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text', date_happened: Time.new(2020, 2, 22, 12, 22, 22, 0)))
          .to eq '_e{15,21}:this is a title|this is a longer text|d:2020-02-22 12:22:22 +0000'
      end
    end

    context 'when having a hostname' do
      it 'serializes the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text', hostname: 'amsterdam'))
          .to eq '_e{15,21}:this is a title|this is a longer text|h:amsterdam'
      end
    end

    context 'when having an aggregation key' do
      it 'serializes the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text', aggregation_key: 'some key'))
          .to eq '_e{15,21}:this is a title|this is a longer text|k:some key'
      end
    end

    context 'when having a source type name' do
      it 'serializes the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text', source_type_name: 'the source'))
          .to eq '_e{15,21}:this is a title|this is a longer text|s:the source'
      end
    end

    context 'when having tags' do
      let(:message_tags) do
        double('message tags')
      end

      before do
        allow(tag_serializer)
          .to receive(:format)
          .with(message_tags)
          .and_return('globaltag1:value1,msgtag2:value2')
      end

      it 'uses the tags serializer correctly' do
        expect(tag_serializer)
          .to receive(:format)
          .with(message_tags)

        subject.format('windmill', 'grinding', tags: message_tags)
      end

      it 'adds the tags to the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text', tags: message_tags))
          .to eq '_e{15,21}:this is a title|this is a longer text|#globaltag1:value1,msgtag2:value2'
      end
    end

    context 'when having a container id' do
      let(:container_id) do
        'in-23'
      end

      it 'adds the container id field correctly' do
        expect(subject.format('this is a title', 'this is a longer text'))
          .to eq '_e{15,21}:this is a title|this is a longer text|c:in-23'
      end

      it 'adds the cardinality field correctly' do
        expect(subject.format('this is a title', 'this is a longer text', cardinality: :low))
          .to eq '_e{15,21}:this is a title|this is a longer text|c:in-23|card:low'
      end
    end

    context 'when having several parameters (hostname, alert_type, priority, source_type, timestamp, tags)' do
      let(:message_tags) do
        double('message tags')
      end

      before do
        allow(tag_serializer)
          .to receive(:format)
          .with(message_tags)
          .and_return('globaltag1:value1,msgtag2:value2')
      end

      it 'serializes the event correctly' do
        expect(subject.format('this is a title', 'this is a longer text',
          hostname: 'amsterdam',
          alert_type: 'warning',
          priority: 'low',
          source_type_name: 'source',
          tags: message_tags,
          timestamp: Time.new(2020, 2, 22, 12, 22, 22, 0))
        ).to eq '_e{15,21}:this is a title|this is a longer text|h:amsterdam|p:low|s:source|t:warning|#globaltag1:value1,msgtag2:value2'
      end
    end

    context '[testing edge cases on title and text]' do
      it 'protects linebreaks on title' do
        expect(subject.format("this is\na title", 'this is a longer text')).to eq '_e{16,21}:this is\na title|this is a longer text'
      end

      it 'protects linebreaks on text' do
        expect(subject.format('this is a title', "this is a\nlonger text")).to eq '_e{15,22}:this is a title|this is a\nlonger text'
      end
    end

    context 'when a shortenable_text_range is provided' do
      let(:prefix) { 'fixed-prefix-' }
      let(:suffix) { '-fixed-suffix' }
      let(:segment) { 'a' * 500 }
      let(:text) { prefix + segment + suffix }
      let(:range) { prefix.length...(prefix.length + segment.length) }

      it 'keeps the whole event untouched when it already fits the budget' do
        expect(subject.format('this is a title', text, shortenable_text_range: range))
          .to eq(subject.format('this is a title', text))
      end

      context 'with a packet limit tighter than the 8KB protocol limit' do
        let(:max_payload_size) { 120 }

        it 'shortens only the designated segment to fit the smallest budget' do
          message = subject.format('this is a title', text, shortenable_text_range: range)

          expect(message.bytesize).to be <= max_payload_size
        end

        it 'always preserves the body text outside of the segment' do
          message = subject.format('this is a title', text, shortenable_text_range: range)

          expect(message).to include(prefix)
          expect(message).to include(suffix)
        end

        it 'keeps the longest possible complete UTF-8 prefix of the segment' do
          message = subject.format('this is a title', text, shortenable_text_range: range)
          declared_text_size = message.match(/_e\{\d+,(\d+)\}:/)[1].to_i
          body = message.split('|', 2)[1]
          segment_part = body[prefix.length...(body.length - suffix.length)]

          expect(segment_part).to eq('a' * (declared_text_size - prefix.bytesize - suffix.bytesize))
          # adding one more character would overflow the budget
          expect(message.bytesize + 1).to be > max_payload_size
        end

        it 'serializes a decodable event whose declared text length matches the actual text' do
          message = subject.format('this is a title', text, shortenable_text_range: range)
          title_size, text_size = message.match(/_e\{(\d+),(\d+)\}:/).captures.map(&:to_i)
          payload = message.sub(/_e\{\d+,\d+\}:/, '')
          decoded_title, remainder = payload[0, title_size], payload[title_size + 1..-1]

          expect(decoded_title).to eq 'this is a title'
          expect(remainder.bytesize).to eq text_size
          expect(remainder).to start_with(prefix)
          expect(remainder).to end_with(suffix)
        end

        context 'when the segment contains newlines and multibyte characters' do
          let(:segment) { ("line\n" * 60) + ('é' * 40) + ('🎉' * 20) }

          it 'accounts for the newline escaping while shortening' do
            message = subject.format('this is a title', text, shortenable_text_range: range)

            expect(message.bytesize).to be <= max_payload_size
            # every backslash in the body belongs to a protected newline escape,
            # the escape sequence is never cut in half while shortening
            body = message.split('|', 2)[1]
            expect(body.scan(/\\/).size).to eq(body.scan('\\n').size)
            expect(body).not_to match(/\\(?!n)/)
          end

          it 'never cuts in the middle of a multibyte UTF-8 character' do
            message = subject.format('this is a title', text, shortenable_text_range: range)

            expect(message.dup.force_encoding(Encoding::UTF_8)).to be_valid_encoding
            expect(message.bytesize).to be <= max_payload_size
          end

          it 'keeps the longest prefix that fits even at a multibyte boundary' do
            message = subject.format('this is a title', text, shortenable_text_range: range)
            text_size = message.match(/_e\{\d+,(\d+)\}:/)[1].to_i

            # the next retained character would need at least one byte; the
            # packet must be within one character of the budget
            expect(max_payload_size - message.bytesize).to be < 4
            expect(message.split('|', 2)[1].bytesize).to eq text_size
          end
        end

        context 'when shortening makes the text length cross a decimal digit boundary' do
          let(:max_payload_size) { 1_000 }
          let(:segment) { "ab\n" * 400 } # 1600 escaped bytes on its own

          it 'recomputes the header length field with the new number of digits' do
            full_message = subject.format('this is a title', text)
            message = subject.format('this is a title', text, shortenable_text_range: range)

            full_text_digits = full_message.match(/_e\{\d+,(\d+)\}:/)[1]
            shortened_text_digits = message.match(/_e\{\d+,(\d+)\}:/)[1]

            expect(full_text_digits.length).to eq 4
            expect(shortened_text_digits.length).to eq 3
            expect(shortened_text_digits.to_i).to eq message.split('|', 2)[1].bytesize
            expect(message.bytesize).to be <= max_payload_size
          end

          it 'reclaims the characters freed by the shorter header (longest fitting prefix)' do
            message = subject.format('this is a title', text, shortenable_text_range: range)
            chosen_text_size = message.match(/_e\{\d+,(\d+)\}:/)[1].to_i

            # brute-force oracle: the longest escaped body any prefix length can fit
            best_body_size = (0..segment.length).map do |char_count|
              candidate = prefix + segment[0, char_count] + suffix
              packet = subject.format('this is a title', candidate)
              packet.bytesize <= max_payload_size ? packet.match(/_e\{\d+,(\d+)\}:/)[1].to_i : nil
            end.compact.max

            expect(chosen_text_size).to eq(best_body_size)
          end
        end

        it 'preserves tags and option fields in full while shortening' do
          allow(tag_serializer)
            .to receive(:format)
            .with(nil)
            .and_return('a-global-tag,another-global')

          message = subject.format('this is a title', text,
            shortenable_text_range: range,
            alert_type: 'warning',
            priority: 'low')

          expect(message).to end_with('|p:low|t:warning|#a-global-tag,another-global')
          expect(message.bytesize).to be <= max_payload_size
        end

        it 'does not build the whole overlong packet repeatedly while searching for the cut point' do
          escaped_arguments = []
          allow(subject).to receive(:escape).and_wrap_original do |original, argument|
            escaped_arguments << argument.dup
            original.call(argument)
          end

          subject.format('this is a title', text, shortenable_text_range: range)

          # the search runs in O(log n) iterations over bounded prefixes: the
          # full length segment is escaped at most once (for the final packet),
          # never once per candidate length
          expect(escaped_arguments.count).to be <= (Math.log2(segment.length) + 3).ceil
          full_length_escapes = escaped_arguments.count { |argument| argument.length == segment.length }
          expect(full_length_escapes).to be <= 1
        end

        context 'when the fixed parts alone do not fit' do
          let(:max_payload_size) { 30 }
          let(:segment) { 'a' * 10 }

          it 'rejects the event even with an empty shortenable segment' do
            expect do
              subject.format('this is a title', text, shortenable_text_range: range)
            end.to raise_error(RuntimeError, /payload is too big/)
          end
        end
      end

      context 'range validation' do
        it 'rejects a range that is not a Range' do
          expect do
            subject.format('title', text, shortenable_text_range: 10)
          end.to raise_error(ArgumentError, /must be a Range/)
        end

        it 'rejects a range outside of the text bounds' do
          expect do
            subject.format('title', text, shortenable_text_range: 5..(text.length + 5))
          end.to raise_error(ArgumentError, /out of the event text bounds/)
        end

        it 'rejects a reversed range' do
          expect do
            subject.format('title', text, shortenable_text_range: 8..3)
          end.to raise_error(ArgumentError, /out of the event text bounds/)
        end

        it 'rejects an empty range that is not positioned at the end of the text' do
          expect do
            subject.format('title', text, shortenable_text_range: 3...3)
          end.to raise_error(ArgumentError, /out of the event text bounds/)
        end

        it 'accepts an exclusive-end range and negative indexes' do
          negative_range = -(segment.length + suffix.length)...(-suffix.length)
          message = subject.format('title', text, shortenable_text_range: negative_range)

          expect(message).to include(prefix)
          expect(message).to include(suffix)
        end

        it 'accepts an empty insertion range at the end of the text' do
          message = subject.format('title', text, shortenable_text_range: text.length...text.length)

          expect(message).to eq(subject.format('title', text))
        end

        it 'accepts an empty range on an empty text' do
          expect(subject.format('title', '', shortenable_text_range: 0...0))
            .to eq('_e{5,0}:title|')
        end
      end
    end
  end
end
