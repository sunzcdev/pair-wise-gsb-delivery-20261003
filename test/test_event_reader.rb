# -*- coding: utf-8 -*-
require 'minitest_helper'
require 'stringio'

# IO that hands out the document in fixed-size byte chunks (down to one byte
# at a time).
class ChunkIO
  def initialize(bytes, size)
    @bytes = bytes.b
    @size = size
    @pos = 0
  end

  def read(length)
    return nil if @pos >= @bytes.bytesize

    chunk = @bytes.byteslice(@pos, length)
    @pos += chunk.bytesize
    chunk
  end

  def getbyte
    return nil if @pos >= @bytes.bytesize

    @bytes.getbyte(@pos).tap { @pos += 1 }
  end
end

# Rebuilds a plain Hash from the structural event stream.
def build_from_events(events)
  root = {}
  events.each do |name, path, value|
    node = descend_for_build(root, path[0...-1], path.last)
    case name
    when :array_start
      node[path.last] = [] unless node[path.last].is_a?(Array)
    when :inline_table_start, :table_enter
      node[path.last] ||= {}
    when :key, :array_element, :inline_table_entry
      node[path.last] = value
    end
  end
  root
end

def descend_for_build(root, path, terminal)
  node = root
  full = path + [terminal]
  path.each_with_index do |segment, index|
    following = full[index + 1]
    if segment.is_a?(Integer)
      node[segment] ||= (following.is_a?(String) ? {} : [])
      node = node[segment]
    else
      node[segment] ||= following.is_a?(Integer) ? [] : {}
      node = node[segment]
    end
  end
  node
end

def collect_strings(object, seen = {})
  return [] if object.nil? || object.is_a?(Symbol) || object.is_a?(Integer) ||
               object.is_a?(Float) || [true, false].include?(object)
  return [] if seen[object.object_id]

  seen[object.object_id] = true
  case object
  when String then [object]
  when Array then object.flat_map { |item| collect_strings(item, seen) }
  when Hash then object.values.flat_map { |item| collect_strings(item, seen) }
  else
    object.instance_variables.flat_map do |ivar|
      collect_strings(object.instance_variable_get(ivar), seen)
    end
  end
end

describe Tomlrb do
  CHUNK_SIZES = [1, 2, 3, 4, 5, 7, 11, 64, 4096].freeze

  DOCS = [
    '',
    'a = 1',
    "a = 1\nb = 2.0\nc = true\n",
    "s = \"caf\u00e9 \u2615 \u4e2d\u6587 \u65e5\u672c\u8a9e\"",
    'line = "tab\there\nnewline"',
    "multi = \"\"\"\nfirst\nsecond\"\"\"",
    "lit = 'C:\\Users\\node\\templates'",
    'arr = [1, 2, [3, 4], {nested = true}]',
    'inline = {a = 1, b = {c = 2}, d = [10, 20]}',
    "[a.b]\nc.d = 3\n",
    "[a]\n[b]\n[a.c]\nx = 1\n",
    "[[fruit]]\nname = \"apple\"\n[fruit.physical]\ncolor = \"red\"\n" \
      "[[fruit.variety]]\nname = \"red\"\n[[fruit.variety]]\n" \
      "name = \"green\"\n[[fruit]]\nname = \"banana\"\n",
    "d = 1979-05-27T07:32:00Z\nlocal = 07:32:00.999\ndate = 1979-05-27\n",
    "\"quoted key\" = 1\n123 = \"numeric key\"\n",
    "hex = 0xDEAD_BEEF\nnan = nan\ninf = inf\nneg = -0.01\n",
    File.read(File.expand_path('example.toml', __dir__), encoding: 'utf-8'),
    File.read(File.expand_path('example-v0.4.0.toml', __dir__), encoding: 'utf-8'),
    File.read(File.expand_path('hard_example.toml', __dir__), encoding: 'utf-8'),
    File.read(File.expand_path('nested_array_of_tables.toml', __dir__), encoding: 'utf-8'),
    File.read(File.expand_path('Cargo.toml', __dir__), encoding: 'utf-8')
  ].freeze

  it 'streams the same structure as Tomlrb.parse for every chunk size' do
    DOCS.each do |doc|
      expected = Tomlrb.parse(doc)
      CHUNK_SIZES.each do |size|
        events = Tomlrb.each_event(ChunkIO.new(doc, size)).to_a
        _(build_from_events(events)).must_equal expected
      end
    end
  end

  it 'works through a #getbyte-only IO one byte at a time' do
    doc = "a = \"\u00e9llo\"\n[[t]]\nx = [1, 2]"
    io = Object.new
    bytes = doc.b
    pos = 0
    io.define_singleton_method(:getbyte) do
      bytes.getbyte(pos).tap { pos += 1 } if pos < bytes.bytesize
    end
    events = Tomlrb.each_event(io).to_a
    _(events.first).must_equal [:key, ['a'], "\u00e9llo"]
    _(events).must_include [:table_declare, ['t', 0], :array_element]
  end

  it 'emits declaration, scope change and kind separately' do
    events = Tomlrb.each_event(StringIO.new("[a.b]\n[[c]]\n")).to_a
    _(events).must_include [:table_enter, ['a'], :implicit]
    _(events).must_include [:table_enter, ['a', 'b'], :standard]
    _(events).must_include [:table_declare, ['a', 'b'], :standard]
    _(events).must_include [:table_leave, ['a', 'b']]
    _(events).must_include [:table_enter, ['c', 0], :array_element]
    _(events).must_include [:table_declare, ['c', 0], :array_element]
  end

  it 'keeps an implicit parent active when a child table is left' do
    events = Tomlrb.each_event(StringIO.new("[a.b]\nx = 1\n[a.c]\ny = 2\n")).to_a
    implicit_a = events.count { |event| event == [:table_enter, ['a'], :implicit] }
    _(implicit_a).must_equal 1
    _(events).must_include [:table_enter, ['a', 'c'], :standard]
  end

  it 'treats array-of-table headers as appending elements' do
    events = Tomlrb.each_event(StringIO.new("[[p]]\nx = 1\n[[p]]\nx = 2\n")).to_a
    _(events).must_include [:table_declare, ['p', 0], :array_element]
    _(events).must_include [:table_declare, ['p', 1], :array_element]
  end

  it 'announces nested value containers with paths and integer indices' do
    events = Tomlrb.each_event(StringIO.new('v = [{a = 10}, 20]')).to_a
    _(events).must_include [:array_start, ['v']]
    _(events).must_include [:inline_table_start, ['v', 0]]
    _(events).must_include [:inline_table_entry, ['v', 0, 'a'], 10]
    _(events).must_include [:inline_table_end, ['v', 0]]
    _(events).must_include [:array_element, ['v', 1], 20]
    _(events).must_include [:array_end, ['v']]
  end

  it 'symbolizes key segments but keeps integer array indices' do
    events = Tomlrb.each_event(StringIO.new("[[p]]\nx = 1"), symbolize_keys: true).to_a
    _(events).must_include [:table_enter, [:p, 0], :array_element]
    _(events).must_include [:key, [:p, 0, :x], 1]
  end

  it 'returns an enumerator without a block' do
    enum = Tomlrb.each_event(StringIO.new('a = 1'))
    _(enum).must_be_kind_of Enumerator
    _(enum.to_a).must_equal [[:key, ['a'], 1]]
  end

  it 'reports EOF-only unterminated constructs at every chunk size' do
    truncated = [
      'a = "unterminated',
      'a = "dangling\\',
      'a = """multi',
      'a = [1, 2',
      'a = {b = 1',
      '[unterminated_header'
    ]
    truncated.each do |doc|
      CHUNK_SIZES.each do |size|
        _{ Tomlrb.each_event(ChunkIO.new(doc, size)).to_a }
          .must_raise(Tomlrb::ParseError)
      end
    end
  end

  it 'keeps a multi-byte-split UTF-8 character valid at every byte cut' do
    doc = 's = "é☕中"'
    expected = Tomlrb.parse(doc)
    (1..doc.bytesize).each do |size|
      events = Tomlrb.each_event(ChunkIO.new(doc, size)).to_a
      _(build_from_events(events)).must_equal expected
    end
  end

  it 'splits escape sequences and multiline strings across chunks' do
    docs = [
      's = "a\tb\u0041\n"',
      "m = \"\"\"\nline one \\\n  continued\"\"\"",
      "l = '''raw\nline'''"
    ]
    docs.each do |doc|
      expected = Tomlrb.parse(doc)
      CHUNK_SIZES.each do |size|
        events = Tomlrb.each_event(ChunkIO.new(doc, size)).to_a
        _(build_from_events(events)).must_equal expected
      end
    end
  end

  it 'does not build an output tree or keep emitted values' do
    huge = (0...200).map { |i| "key#{i} = \"#{'z' * 500}\"" }.join("\n")
    scanner = Tomlrb::StreamingScanner.new(StringIO.new(huge))
    parser = Tomlrb::EventParser.new(scanner, consumer: ->(_) {})
    handler = parser.parse

    _(handler.instance_variables).wont_include :@output
    retained = collect_strings(handler)
    _((retained.map(&:bytesize).max || 0)).must_be :<=, 64
  end

  it 'releases a single very large value right after its event' do
    huge = "data = \"#{'z' * 100_000}\""
    seen = nil
    scanner = Tomlrb::StreamingScanner.new(StringIO.new(huge))
    parser = Tomlrb::EventParser.new(scanner, consumer: ->(event) { seen = event })
    handler = parser.parse

    _(seen).must_equal [:key, ['data'], 'z' * 100_000]
    retained = collect_strings(handler)
    _((retained.map(&:bytesize).max || 0)).must_be :<=, 64
  end
end
