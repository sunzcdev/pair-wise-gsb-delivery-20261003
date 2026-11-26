# frozen_string_literal: true

require 'spec_helper'

# Integration coverage for events whose body contains a caller-designated
# segment that may be shortened to make the serialized event fit a single
# outbound packet. A local sending double (FakeUDPSocket) is used with a
# packet limit below the 8KB protocol event limit.
describe 'event body shortening', integration: true do
  let(:socket) { FakeUDPSocket.new(copy_message: true) }

  let(:packet_limit) { 600 }

  subject do
    Datadog::Statsd.new('localhost', 1234,
      telemetry_enable: false,
      origin_detection: false,
      single_thread: single_thread,
      buffer_max_payload_size: packet_limit)
  end

  before do
    allow(Socket).to receive(:new).and_return(socket)
    allow(UDPSocket).to receive(:new).and_return(socket)
  end

  after do
    subject.close
  end

  # decodes a DogStatsD event line "_e{title_len,text_len}:title|text|..."
  # using the declared byte lengths, exactly like the agent would
  def decode_event(line)
    header = line.match(/\A_e\{(\d+),(\d+)\}:/)
    expect(header).not_to be_nil

    title_len = header[1].to_i
    text_len = header[2].to_i
    payload = line[header[0].bytesize..-1]

    title = payload.byteslice(0, title_len)
    separator = payload.byteslice(title_len, 1)
    text = payload.byteslice(title_len + 1, text_len)
    tail = payload.byteslice(title_len + 1 + text_len,
      payload.bytesize - title_len - 1 - text_len)

    expect(separator).to eq('|')
    expect(title.dup.force_encoding(Encoding::UTF_8)).to be_valid_encoding
    expect(text.dup.force_encoding(Encoding::UTF_8)).to be_valid_encoding

    { title: title, text: text, tail: tail || '' }
  end

  shared_examples 'a shortenable event sender' do
    it 'shortens the designated segment and keeps everything else intact' do
      prefix = "preamble\n"          # a newline outside of the segment: must be kept
      suffix = '-tail-end'           # fixed suffix outside of the segment: must be kept
      segment = ("row\n" * 120) + ('é' * 30) + ('🎉' * 20)
      text = prefix + segment + suffix
      range = prefix.length...(prefix.length + segment.length)

      full_size = subject.send(:serializer).to_event('the-title', text).bytesize
      expect(full_size).to be > packet_limit

      subject.event('the-title', text,
        shortenable_text_range: range,
        alert_type: 'warning',
        tags: ['team:ruby', 'area:events'])
      subject.count('ordinary.counter', 42)
      subject.flush(sync: true)

      packets = []
      while (packet = socket.recv)
        packets << packet[0]
      end

      # both lines are delivered (possibly in separate packets), neither one is lost
      event_packet = packets.find { |p| p.start_with?('_e{') }
      expect(event_packet).not_to be_nil
      expect(event_packet.bytesize).to be <= packet_limit

      event_line = event_packet.split("\n").find { |line| line.start_with?('_e{') }

      decoded = decode_event(event_line)
      expect(decoded[:title]).to eq('the-title')

      # the length fields let the agent decode the (shortened) body exactly
      declared_text_len = event_line.match(/_e\{\d+,(\d+)\}:/)[1].to_i
      expect(decoded[:text].bytesize).to eq(declared_text_len)

      # text outside of the shortenable segment is preserved in full, including
      # its protected newline escaping ("\n" serialized as the 2 chars \ + n)
      expect(decoded[:text]).to start_with('preamble' + '\\' + 'n')
      expect(decoded[:text]).to end_with(suffix)

      # the shortened segment only contains whole UTF-8 characters
      expect(decoded[:text].dup.force_encoding(Encoding::UTF_8)).to be_valid_encoding

      # tags and option fields are kept in full in the fixed tail
      expect(decoded[:tail]).to eq('|t:warning|#team:ruby,area:events')

      # the ordinary counter following the event is delivered untouched
      all_lines = packets.flat_map { |packet| packet.split("\n") }
      expect(all_lines).to include('ordinary.counter:42|c')
    end

    it 'crosses the decimal digit boundary of the text length field while shortening' do
      prefix = 'P'
      suffix = 'S'
      segment = "ab\n" * 400 # 1200 raw characters, 1600 escaped bytes
      text = prefix + segment + suffix
      range = 1...(text.length - 1)

      unscaled_event = subject.send(:serializer).to_event('T', text)
      expect(unscaled_event.match(/_e\{\d+,(\d+)\}:/)[1].length).to eq(4)

      subject.event('T', text, shortenable_text_range: range)
      subject.flush(sync: true)

      event_line = socket.recv[0]
      expect(event_line.bytesize).to be <= packet_limit

      text_field = event_line.match(/_e\{\d+,(\d+)\}:/)[1]
      expect(text_field.length).to eq(3)

      decoded = decode_event(event_line)
      expect(decoded[:text].bytesize).to eq(text_field.to_i)
      expect(decoded[:text]).to start_with(prefix)
      expect(decoded[:text]).to end_with(suffix)
    end

    it 'rejects an event whose fixed parts alone do not fit without polluting the batch' do
      huge_title = 'X' * (packet_limit + 50)
      text = 'body-' + ('y' * 200) + '-end'
      range = 5...(text.length - 4)

      expect do
        subject.event(huge_title, text, shortenable_text_range: range)
      end.to raise_error(RuntimeError, /payload is too big/)

      # a following metric must still be sent normally
      subject.count('after.rejection', 7)
      subject.flush(sync: true)

      packet = socket.recv[0]
      expect(packet).to eq('after.rejection:7|c')
      expect(packet.bytesize).to be <= packet_limit
      expect(socket.recv).to be_nil
    end

    it 'keeps the historical behavior when no shortenable range is given' do
      text = 'plain body that fits'

      subject.event('plain-title', text)
      subject.flush(sync: true)

      expect(socket.recv[0]).to eq('_e{11,20}:plain-title|plain body that fits')
    end
  end

  context 'in single threaded mode' do
    let(:single_thread) { true }

    it_behaves_like 'a shortenable event sender'
  end

  context 'in multi threaded mode' do
    let(:single_thread) { false }

    it_behaves_like 'a shortenable event sender'
  end
end
