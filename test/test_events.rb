require 'minitest_helper'
require 'weakref'

# An IO that never returns more than +max+ bytes per read call, to prove
# tokens are assembled correctly no matter where chunk boundaries fall
# (inside a UTF-8 character, an escape sequence or a multi-line string).
class DripIO
  def initialize(string, max: 3)
    @data = string.b
    @max = max
    @pos = 0
  end

  def read(_length = nil)
    return nil if @pos >= @data.bytesize

    size = 1 + (@pos % @max)
    chunk = @data.byteslice(@pos, size)
    @pos += size
    chunk
  end
end

describe Tomlrb do
  def collect_events(input, **options)
    events = []
    Tomlrb.each_event(input, **options) { |event| events << event }
    events
  end

  # Consumer-side reconstruction of the document tree from events, used
  # to prove the event stream carries the same information as
  # Tomlrb.parse.
  def build_tree(events)
    root = {}
    navigate = lambda do |path|
      path.reduce(root) do |node, key|
        current = node[key]
        current.is_a?(Array) ? current.last : (node[key] ||= {})
      end
    end
    events.each do |type, path, value|
      case type
      when :table
        navigate.call(path)
      when :array_of_tables
        (navigate.call(path[0..-2])[path[-1]] ||= []) << {}
      when :value
        navigate.call(path[0..-2])[path[-1]] = value
      end
    end
    root
  end

  VALID_FILES = %w[
    example.toml
    example-v0.4.0.toml
    hard_example.toml
    Cargo.toml
    inf_in_keys.toml
    nested_array_of_tables.toml
  ]

  VALID_FILES.each do |file|
    it "emits events matching Tomlrb.parse for #{file} when fed byte by byte" do
      content = File.read("./test/#{file}", encoding: 'utf-8')
      bulk = collect_events(StringIO.new(content))
      drip1 = collect_events(DripIO.new(content, max: 1))
      drip3 = collect_events(DripIO.new(content, max: 3))

      _(drip1).must_equal bulk
      _(drip3).must_equal bulk
      _(build_tree(drip1)).must_equal Tomlrb.parse(content)
    end
  end

  it 'requires a block' do
    _{ Tomlrb.each_event('a = 1') }.must_raise(ArgumentError)
  end

  it 'accepts a custom chunk size' do
    events = collect_events(StringIO.new("a = 1\n"), chunk_size: 1)
    _(events).must_equal [[:value, ['a'], 1]]
  end

  it 'emits declarations, paths and completed values in document order' do
    doc = <<~TOML
      [a.b]
      x = 1
      [a]
      y = [1, {z = 2}, [3, 4]]
      [[a.items]]
      name = "first"
      [[a.items]]
      name = "second"
    TOML

    _(collect_events(doc)).must_equal [
      [:table, %w[a b]],
      [:value, %w[a b x], 1],
      [:table, %w[a]],
      [:value, %w[a y], [1, {'z' => 2}, [3, 4]]],
      [:array_of_tables, %w[a items]],
      [:value, %w[a items name], 'first'],
      [:array_of_tables, %w[a items]],
      [:value, %w[a items name], 'second']
    ]
  end

  it 'declares a normal table after values under its path were emitted' do
    events = collect_events("[a.b]\nx = 1\n[a]\ny = 2\n")
    _(events).must_equal [
      [:table, %w[a b]],
      [:value, %w[a b x], 1],
      [:table, %w[a]],
      [:value, %w[a y], 2]
    ]
  end

  it 'does not emit table-closed events' do
    events = collect_events("[a]\nx = 1\n[b]\ny = 2\n")
    _(events.map(&:first)).must_equal %i[table value table value]
  end

  it 'delivers nested containers as a single completed value' do
    events = collect_events("x = [1, [2, 3], {a = {b = 4}}]\n")
    _(events).must_equal [[:value, ['x'], [1, [2, 3], {'a' => {'b' => 4}}]]]
  end

  it 'handles UTF-8 split across chunks' do
    doc = "title = \"中国 🚀\"\n\"clé 🔑\" = 'värdö'\n"
    _(collect_events(DripIO.new(doc, max: 1))).must_equal collect_events(doc)
    _(build_tree(collect_events(DripIO.new(doc, max: 1)))).must_equal Tomlrb.parse(doc)
  end

  it 'handles escape sequences split across chunks' do
    doc = "esc = \"a\\nb\\t\\u4e2d\\\\\"\n"
    _(collect_events(DripIO.new(doc, max: 1))).must_equal collect_events(doc)
    _(build_tree(collect_events(DripIO.new(doc, max: 1)))).must_equal Tomlrb.parse(doc)
  end

  it 'handles multi-line strings split across chunks' do
    doc = "a = \"\"\"\nline1\nline2 \\\ncont\"\"\"\nb = '''\nraw\n'''\n"
    _(collect_events(DripIO.new(doc, max: 1))).must_equal collect_events(doc)
    _(build_tree(collect_events(DripIO.new(doc, max: 1)))).must_equal Tomlrb.parse(doc)
  end

  it 'handles datetimes split across chunks' do
    doc = "dt = 1979-05-27T07:32:00.999-07:00\nd = 1979-05-27\nt = 07:32:00\n"
    _(collect_events(DripIO.new(doc, max: 1))).must_equal collect_events(doc)
    _(build_tree(collect_events(DripIO.new(doc, max: 1)))).must_equal Tomlrb.parse(doc)
  end

  it 'handles CRLF line endings split across chunks' do
    doc = "a = 1\r\n[b]\r\nx = \"y\"\r\n"
    _(collect_events(DripIO.new(doc, max: 1))).must_equal collect_events(doc)
    _(build_tree(collect_events(DripIO.new(doc, max: 1)))).must_equal Tomlrb.parse(doc)
  end

  it 'handles empty input' do
    _(collect_events('')).must_equal []
  end

  it 'detects spaced array-of-tables brackets' do
    _{ collect_events(DripIO.new("[ [a] ]\n", max: 2)) }.must_raise(Tomlrb::ParseError)
    _{ collect_events(DripIO.new("[ [a]]\n", max: 2)) }.must_raise(Tomlrb::ParseError)
    _{ collect_events(DripIO.new("[[a] ]\n", max: 2)) }.must_raise(Tomlrb::ParseError)
  end

  it 'keeps a bounded input window on large documents' do
    doc = (1..3000).map { |i| "key#{i} = \"value #{i} #{'pad' * 20}\"" }.join("\n") + "\n"
    _(doc.bytesize).must_be :>, Tomlrb::StreamScanner::TRIM_THRESHOLD

    bulk = collect_events(doc)
    _(collect_events(DripIO.new(doc, max: 3))).must_equal bulk

    scanner = Tomlrb::StreamScanner.new(DripIO.new(doc, max: 100))
    max_buffer = 0
    refill = scanner.method(:refill)
    scanner.define_singleton_method(:refill) do
      refill.call
      max_buffer = [@buffer.bytesize, max_buffer].max
    end
    scanner.next_token while !scanner.instance_variable_get(:@eos)

    _(max_buffer).must_be :<, doc.bytesize
  end

  it 'raises a clear error for an unterminated basic string at EOF' do
    error = _{ collect_events(DripIO.new('x = "abc', max: 1)) }.must_raise(Tomlrb::ParseError)
    _(error.message).must_match(/unterminated/i)
  end

  it 'raises a clear error for an unterminated multi-line string at EOF' do
    error = _{ collect_events(DripIO.new("x = \"\"\"abc\n", max: 2)) }.must_raise(Tomlrb::ParseError)
    _(error.message).must_match(/unterminated/i)
  end

  it 'raises a clear error for an unterminated literal string at EOF' do
    error = _{ collect_events("x = 'abc") }.must_raise(Tomlrb::ParseError)
    _(error.message).must_match(/unterminated/i)
  end

  it 'raises a clear error for truncated UTF-8 at EOF' do
    error = _{ collect_events(DripIO.new("x = \"ab\xE4\xB8".b, max: 2)) }.must_raise(Tomlrb::ParseError)
    _(error.message).must_match(/UTF-8/i)
  end

  it 'raises a clear error for invalid UTF-8 bytes' do
    error = _{ collect_events("x = \"\xFF\xFE\"".b) }.must_raise(Tomlrb::ParseError)
    _(error.message).must_match(/UTF-8/i)
  end

  it 'still detects duplicate pair keys' do
    _{ collect_events("a = 1\na = 2\n") }.must_raise(Tomlrb::ParseError)
  end

  it 'still detects a value extended by a dotted key' do
    _{ collect_events("a = 1\na.b = 2\n") }.must_raise(Tomlrb::ParseError)
  end

  it 'still detects duplicate table declarations' do
    _{ collect_events("[a]\n[a]\n") }.must_raise(Tomlrb::ParseError)
  end

  it 'still detects a normal table defined over an array of tables' do
    _{ collect_events("[[a]]\n[a]\n") }.must_raise(Tomlrb::ParseError)
  end

  it 'still detects an array of tables defined over a normal table' do
    _{ collect_events("[a]\n[[a]]\n") }.must_raise(Tomlrb::ParseError)
  end

  it 'still raises on the bundled error.toml' do
    _{ Tomlrb.each_event(File.open('./test/error.toml', 'r:utf-8')) { } }.must_raise(Tomlrb::ParseError)
  end

  it 'symbolizes event paths with symbolize_keys: true' do
    events = collect_events("[a]\nb = 1\n", symbolize_keys: true)
    _(events).must_equal [[:table, [:a]], [:value, [:a, :b], 1]]
  end

  it 'keeps only key names and container kinds, not values' do
    doc = <<~TOML
      big = "#{'x' * 100_000}"
      arr = [1, 2, 3]
      inline = {y = 2}
      [table]
      z = 3
      [[aot]]
      n = 1
      [[aot]]
      n = 2
    TOML
    handler = Tomlrb::EventHandler.new(->(_event) {})
    parser = Tomlrb::Parser.new(Tomlrb::StreamScanner.new(StringIO.new(doc)), handler: handler)
    parser.parse

    walk = lambda do |node, &recurse|
      case node
      when Hash
        node.each do |key, value|
          _(key).must_be_kind_of String
          recurse.call(value, &recurse)
        end
      when Tomlrb::EventHandler::ArrayShape
        recurse.call(node.element, &recurse)
      else
        _(node).must_equal Tomlrb::EventHandler::VALUE
      end
    end
    walk.call(handler.instance_variable_get(:@root), &walk)
  end

  it 'lets the consumer release processed values' do
    ref = nil
    Tomlrb.each_event(StringIO.new("big = \"#{'x' * 200_000}\"")) do |event|
      ref = WeakRef.new(event[2]) if event[0] == :value
    end
    GC.start
    _(!!ref.weakref_alive?).must_equal false
  end
end
