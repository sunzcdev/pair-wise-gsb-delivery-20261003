require 'minitest_helper'

describe 'structured key declaration sources' do
  def parse_conflict(source)
    Tomlrb.parse(source)
    flunk 'expected a Tomlrb::Key::KeyConflict'
  rescue Tomlrb::Key::KeyConflict => error
    error
  end

  it 'keeps KeyConflict a ParseError subclass' do
    _(Tomlrb::Key::KeyConflict.ancestors).must_include Tomlrb::ParseError
  end

  it 'reports both locations for a duplicate value' do
    error = parse_conflict("a = 1\na = 2\n")
    _(error.key_name).must_equal 'a'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _(error.current.location.line).must_equal 2
    _(error.previous.location.line).must_equal 1
    _(error.message).must_include 'line 2, column 1'
    _(error.message).must_include 'line 1, column 1'
  end

  it 'reports the conflicting key of a dotted re-assignment' do
    error = parse_conflict("a.b = 1\na.b = 2\n")
    _(error.key_name).must_equal 'b'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _([error.current.location.line, error.current.location.column]).must_equal [2, 3]
    _([error.previous.location.line, error.previous.location.column]).must_equal [1, 3]
  end

  it 'labels an implicit parent table and the previous value' do
    error = parse_conflict("a.b = 1\na.b.c = 2\n")
    _(error.key_name).must_equal 'b'
    _(error.current.kind).must_equal :implicit_table
    _(error.previous.kind).must_equal :value
    _(error.message).must_include 'an implicit parent table'
    _(error.message).must_include 'a value'
  end

  it 'labels the two explicit table headers' do
    error = parse_conflict("[fruit]\n[fruit]\n")
    _(error.key_name).must_equal 'fruit'
    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :explicit_table
    _(error.current.location.line).must_equal 2
    _(error.previous.location.line).must_equal 1
  end

  it 'flags the closed inline table value used as a table parent' do
    error = parse_conflict("point = { x = 1 }\n[point.x]\ny = 2\n")
    _(error.key_name).must_equal 'point'
    _(error.previous.kind).must_equal :inline_table
    _(error.current.kind).must_equal :implicit_table
    _(error.previous.location.line).must_equal 1
    _(error.current.location.line).must_equal 2
  end

  it 'labels the outer closed inline table used as a dotted parent' do
    error = parse_conflict("point = { x = 1 }\npoint.x = 2\n")
    _(error.key_name).must_equal 'point'
    _(error.previous.kind).must_equal :inline_table
    _(error.current.kind).must_equal :implicit_table
  end

  it 'labels a closed inline table node against a new table header' do
    error = parse_conflict("[p]\nq = { inner = 1 }\n[p.q]\nz = 1\n")
    _(error.key_name).must_equal 'q'
    _(error.previous.kind).must_equal :inline_table
    _(error.current.kind).must_equal :explicit_table
    _(error.previous.location.line).must_equal 2
    _(error.current.location.line).must_equal 3
  end

  it 'labels the closed inline table when redefined by a header' do
    error = parse_conflict("point = { x = 1 }\n[point]\n")
    _(error.key_name).must_equal 'point'
    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :inline_table
  end

  it 'reports both sources of a duplicated inline key' do
    error = parse_conflict("t = { x = 1, x = 2 }\n")
    _(error.key_name).must_equal 'x'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _(error.current.location.column).must_equal 14
    _(error.previous.location.column).must_equal 7
  end

  it 'does not treat a.b and quoted "a.b" as the same path' do
    result = Tomlrb.parse(%Q{"a.b" = 1\na.b = 2\n})
    _(result).must_equal({ 'a.b' => 1, 'a' => { 'b' => 2 } })
  end

  it 'keeps two quoted "a.b" declarations conflicting' do
    error = parse_conflict(%Q{"a.b" = 1\n"a.b" = 2\n})
    _(error.key_name).must_equal 'a.b'
    _(error.current.location.line).must_equal 2
    _(error.previous.location.line).must_equal 1
  end

  it 'allows the same key in different array-of-tables instances' do
    result = Tomlrb.parse(
      "[[fruit]]\nname = 'apple'\n[[fruit]]\nname = 'banana'\n"
    )
    _(result).must_equal({
      'fruit' => [{ 'name' => 'apple' }, { 'name' => 'banana' }]
    })
  end

  it 'still reports duplicates inside one array-of-tables instance' do
    error = parse_conflict("[[fruit]]\nname = 'apple'\nname = 'banana'\n")
    _(error.key_name).must_equal 'name'
    _(error.current.location.line).must_equal 3
    _(error.previous.location.line).must_equal 2
  end

  it 'isolates sub-tables of different array-of-tables instances' do
    source = <<~TOML
      [[fruit]]
      [fruit.info]
      color = 'red'
      [[fruit]]
      [fruit.info]
      color = 'green'
    TOML
    result = Tomlrb.parse(source)
    _(result).must_equal({
      'fruit' => [
        { 'info' => { 'color' => 'red' } },
        { 'info' => { 'color' => 'green' } }
      ]
    })
  end

  it 'allows legal super-table declarations after sub-tables' do
    result = Tomlrb.parse("[x.y.z]\nw = 1\n[x]\nv = 2\n")
    _(result).must_equal({
      'x' => { 'y' => { 'z' => { 'w' => 1 } }, 'v' => 2 }
    })
  end

  it 'interleaves a legal super-table and a repeated header' do
    source = <<~TOML
      [server]
      host = "localhost"
      [server.tls]
      enabled = true
      [server]
      host = "example.com"
    TOML
    error = parse_conflict(source)
    _(error.key_name).must_equal 'server'
    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :explicit_table
    _(error.current.location.line).must_equal 5
    _(error.previous.location.line).must_equal 1
  end

  it 'accepts a backward-defined super-table between assignments' do
    source = "name = 1\n[server.tls]\nenabled = true\n[server]\nhost = 2\n"
    result = Tomlrb.parse(source)
    _(result).must_equal({
      'name' => 1,
      'server' => { 'tls' => { 'enabled' => true }, 'host' => 2 }
    })
  end

  it 'switches array tables before a later inline extension conflict' do
    source = <<~TOML
      [[products]]
      name = "one"
      meta = { version = 1 }
      [[products]]
      name = "two"
      meta.version = 2
    TOML
    # The inline table belongs to the first instance; the same keys in the
    # second instance live in a fresh subtree and must not conflict.
    result = Tomlrb.parse(source)
    _(result).must_equal({
      'products' => [
        { 'name' => 'one', 'meta' => { 'version' => 1 } },
        { 'name' => 'two', 'meta' => { 'version' => 2 } }
      ]
    })
  end

  it 'flags extending an inline table inside the same array instance' do
    source = <<~TOML
      [[products]]
      meta = { version = 1 }
      meta.version = 2
    TOML
    error = parse_conflict(source)
    _(error.key_name).must_equal 'meta'
    _(error.current.kind).must_equal :implicit_table
    _(error.previous.kind).must_equal :inline_table
    _(error.current.location.line).must_equal 3
    _(error.previous.location.line).must_equal 2
  end

  it 'tracks line numbers across multiline values' do
    source = "a = \"\"\"\nline two\nline three\n\"\"\"\na = 2\n"
    error = parse_conflict(source)
    _(error.previous.location.line).must_equal 1
    _(error.current.location.line).must_equal 5
  end
end
