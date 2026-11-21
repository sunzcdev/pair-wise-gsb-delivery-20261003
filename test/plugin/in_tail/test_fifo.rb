require_relative '../../helper'

require 'fluent/plugin/in_tail'

class IntailFIFO < Test::Unit::TestCase
  sub_test_case '#read_line' do
    test 'returns lines splitting per `\n`' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      text = ("test\n" * 3).force_encoding(Encoding::ASCII_8BIT)
      fifo << text
      lines = []
      fifo.read_lines(lines)
      assert_equal Encoding::ASCII_8BIT, lines[0].encoding
      assert_equal ["test\n", "test\n", "test\n"], lines
    end

    test 'concat line when line is separated' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      text = ("test\n" * 3 + 'test').force_encoding(Encoding::ASCII_8BIT)
      fifo << text
      lines = []
      fifo.read_lines(lines)
      assert_equal Encoding::ASCII_8BIT, lines[0].encoding
      assert_equal ["test\n", "test\n", "test\n"], lines

      fifo << "2\n"
      fifo.read_lines(lines)
      assert_equal Encoding::ASCII_8BIT, lines[0].encoding
      assert_equal ["test\n", "test\n", "test\n", "test2\n"], lines
    end

    test 'returns lines which convert encoding' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, Encoding::UTF_8)
      text = ("test\n" * 3).force_encoding(Encoding::ASCII_8BIT)
      fifo << text
      lines = []
      fifo.read_lines(lines)
      assert_equal Encoding::UTF_8, lines[0].encoding
      assert_equal ["test\n", "test\n", "test\n"], lines
    end

    test 'reads lines as from_encoding' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::UTF_8, $log, nil, Encoding::ASCII_8BIT)
      text = ("test\n" * 3).force_encoding(Encoding::UTF_8)
      fifo << text
      lines = []
      fifo.read_lines(lines)
      assert_equal Encoding::ASCII_8BIT, lines[0].encoding
      assert_equal ["test\n", "test\n", "test\n"], lines
    end

    sub_test_case 'when it includes multi byte chars' do
      test 'handles it as ascii_8bit' do
        fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
        text = ("てすと\n" * 3).force_encoding(Encoding::ASCII_8BIT)
        fifo << text
        lines = []
        fifo.read_lines(lines)
        assert_equal Encoding::ASCII_8BIT, lines[0].encoding
        assert_equal ["てすと\n", "てすと\n", "てすと\n"].map { |e| e.force_encoding(Encoding::ASCII_8BIT) }, lines
      end

      test 'replaces character with ? when convert error happens' do
        fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::UTF_8, $log, nil, Encoding::ASCII_8BIT)
        text = ("てすと\n" * 3).force_encoding(Encoding::UTF_8)
        fifo << text
        lines = []
        fifo.read_lines(lines)
        assert_equal Encoding::ASCII_8BIT, lines[0].encoding
        assert_equal ["???\n", "???\n", "???\n"].map { |e| e.force_encoding(Encoding::ASCII_8BIT) }, lines
      end
    end

    test 'returns nothing when buffer is empty' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      lines = []
      fifo.read_lines(lines)
      assert_equal [], lines

      text = "test\n" * 3
      fifo << text
      fifo.read_lines(lines)
      assert_equal ["test\n", "test\n", "test\n"], lines

      lines = []
      fifo.read_lines(lines)
      assert_equal [], lines
    end

    data('bigger than max_line_size', [
      ["test test test\n" * 3],
      [],
    ])
    data('less than or equal to max_line_size', [
      ["test\n" * 2],
      ["test\n", "test\n"],
    ])
    data('mix', [
      ["test test test\ntest\ntest test test\ntest\ntest test test\n"],
      ["test\n", "test\n"],
    ])
    data('mix and multiple', [
      [
        "test test test\ntest\n",
        "test",
        " test test\nt",
        "est\nt"
      ],
      ["test\n", "test\n"],
    ])
    data('remaining data bigger than max_line_size should be discarded', [
      [
        "test\nlong line still not having EOL",
        "following texts to the previous long line\ntest\n",
      ],
      ["test\n", "test\n"],
    ])
    test 'return lines only that size is less than or equal to max_line_size' do |(input_texts, expected)|
      max_line_size = 5
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, max_line_size)
      lines = []

      input_texts.each do |text|
        fifo << text.force_encoding(Encoding::ASCII_8BIT)
        fifo.read_lines(lines)
        # The size of remaining buffer (i.e. a line still not having EOL) must not exceed max_line_size.
        assert { fifo.buffer.bytesize <= max_line_size }
      end

      assert_equal expected, lines
    end
  end

  sub_test_case 'byte range' do
    def byte_range(line)
      Fluent::Plugin::TailInput::ByteRange.of(line)
    end

    test 'attaches absolute byte ranges of the emitted lines' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, nil, track_byte_range: true)
      fifo << "test\n"
      fifo.sync_base(5)
      lines = []
      fifo.read_lines(lines)

      assert_equal 1, lines.size
      assert_equal [0, 5], byte_range(lines[0])
      assert_equal 'test', lines[0].chomp
    end

    test 'keeps ranges of lines split across chunks and reads' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, nil, track_byte_range: true)
      fifo << 'foo'
      fifo.sync_base(3)
      lines = []
      fifo.read_lines(lines)
      assert_equal [], lines

      fifo << "bar\nbaz\n"
      fifo.read_lines(lines)
      assert_equal [[0, 7], [7, 11]], lines.map { |line| byte_range(line) }
      assert_equal ['foobar', 'baz'], lines.map(&:chomp)
    end

    test 'counts raw bytes before encoding conversion' do
      # UTF-8 multibyte characters are read as ASCII-8BIT chunks, then converted
      # to UTF-8. Each 'て' is 3 bytes in the file.
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, Encoding::UTF_8, track_byte_range: true)
      text = "てすと\n".dup.force_encoding(Encoding::ASCII_8BIT)
      # Feed it one byte at a time, splitting the multibyte characters.
      text.each_byte do |byte|
        fifo << byte.chr.force_encoding(Encoding::ASCII_8BIT)
      end
      fifo.sync_base(text.bytesize)
      lines = []
      fifo.read_lines(lines)

      assert_equal 1, lines.size
      assert_equal Encoding::UTF_8, lines[0].encoding
      assert_equal [0, text.bytesize], byte_range(lines[0])
      assert_equal 10, text.bytesize
    end

    test 'includes the full CRLF in the range' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, nil, track_byte_range: true)
      fifo << "a\r\nb\r\n"
      fifo.sync_base(6)
      lines = []
      fifo.read_lines(lines)

      assert_equal [[0, 3], [3, 6]], lines.map { |line| byte_range(line) }
    end

    test 'does not split a CRLF whose CR and LF arrive in different reads' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, nil, nil, track_byte_range: true)
      fifo << "abc\r"
      fifo.sync_base(4)
      lines = []
      fifo.read_lines(lines)
      assert_equal [], lines

      fifo << "\ndef\n"
      fifo.read_lines(lines)
      assert_equal [[0, 5], [5, 9]], lines.map { |line| byte_range(line) }
      assert_equal ['abc', 'def'], lines.map(&:chomp)
    end

    test 'counts bytes of skipped long lines so later ranges stay correct' do
      max_line_size = 6
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, max_line_size, nil, track_byte_range: true)
      fifo << "short\ntoolong"
      fifo.sync_base(13)
      lines = []
      # "toolong" already exceeds the limit and is discarded while still
      # waiting for its line ending.
      fifo.read_lines(lines)
      assert_equal ['short'], lines.map(&:chomp)

      fifo << "line\nnext\n"
      fifo.read_lines(lines)
      assert_equal ['short', 'next'], lines.map(&:chomp)
      assert_equal [[0, 6], [18, 23]], lines.map { |line| byte_range(line) }
    end

    test 'attaches nothing when byte range tracking is disabled' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      fifo << "test\n"
      lines = []
      fifo.read_lines(lines)

      assert_nil byte_range(lines[0])
    end
  end

  sub_test_case '#<<' do
    test 'does not make any change about encoding to an argument' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      text = ("test\n" * 3).force_encoding(Encoding::UTF_8)

      assert_equal Encoding::UTF_8, text.encoding
      fifo << text
      assert_equal Encoding::UTF_8, text.encoding
    end
  end

  sub_test_case '#reading_bytesize' do
    test 'returns buffer size' do
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log)
      text = "test\n" * 3 + 'test'
      fifo << text

      assert_equal text.bytesize, fifo.reading_bytesize
      lines = []
      fifo.read_lines(lines)
      assert_equal ["test\n", "test\n", "test\n"], lines

      assert_equal 'test'.bytesize, fifo.reading_bytesize
      fifo << "2\n"
      fifo.read_lines(lines)
      assert_equal ["test\n", "test\n", "test\n", "test2\n"], lines

      assert_equal 0, fifo.reading_bytesize
    end

    test 'returns the entire line size even if the size is over max_line_size' do
      max_line_size = 20
      fifo = Fluent::Plugin::TailInput::TailWatcher::FIFO.new(Encoding::ASCII_8BIT, $log, max_line_size)
      lines = []

      text = "long line still not having EOL"
      fifo << text
      fifo.read_lines(lines)
      assert_equal [], lines
      assert_equal 0, fifo.buffer.bytesize
      assert_equal text.bytesize, fifo.reading_bytesize

      text2 = " following texts"
      fifo << text2
      fifo.read_lines(lines)
      assert_equal [], lines
      assert_equal 0, fifo.buffer.bytesize
      assert_equal text.bytesize + text2.bytesize, fifo.reading_bytesize

      text3 = " end of the line\n"
      fifo << text3
      fifo.read_lines(lines)
      assert_equal [], lines
      assert_equal 0, fifo.buffer.bytesize
      assert_equal 0, fifo.reading_bytesize
    end
  end
end
