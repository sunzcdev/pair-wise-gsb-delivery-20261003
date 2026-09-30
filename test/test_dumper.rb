require 'minitest_helper'
require 'time'

describe Tomlrb::Dumper do
  def roundtrip(config)
    Tomlrb.parse(Tomlrb.dump(config))
  end

  it 'dumps an empty document' do
    _(Tomlrb.dump({})).must_equal ''
    _(roundtrip({})).must_equal({})
  end

  it 'roundtrips a plain hash of scalars and nested tables' do
    config = {
      'title' => 'TOML Example',
      'owner' => {
        'name' => 'Tom Preston-Werner',
        'dob'  => Time.utc(1979, 5, 27, 7, 32, 0)
      },
      'database' => {
        'server' => '192.168.1.1',
        'ports' => [8001, 8001, 8002],
        'connection_max' => 5000,
        'enabled' => true,
        'ratio' => 3.14
      }
    }

    _(roundtrip(config)).must_equal config
  end

  it 'keeps integers distinct from floats' do
    config = {'integer' => 1, 'float' => 1.0, 'exponent' => 6.022e23,
              'neg_zero' => -0.0, 'big' => 9_007_199_254_740_993}

    dumped = Tomlrb.dump(config)
    _(dumped).must_include 'integer = 1'
    _(dumped).must_include 'float = 1.0'

    result = roundtrip(config)
    _(result['integer']).must_be_kind_of Integer
    _(result['float']).must_be_kind_of Float
    _(result).must_equal config
  end

  it 'dumps float keywords' do
    config = {'pinf' => Float::INFINITY, 'ninf' => -Float::INFINITY,
              'nan' => Float::NAN}

    dumped = Tomlrb.dump(config)
    _(dumped).must_include 'pinf = inf'
    _(dumped).must_include 'ninf = -inf'
    _(dumped).must_include 'nan = nan'
    _(roundtrip(config)['pinf']).must_equal Float::INFINITY
  end

  it 'roundtrips local dates, times and date-times' do
    config = {
      'date' => Tomlrb::LocalDate.new(1979, 5, 27),
      'time' => Tomlrb::LocalTime.new(7, 32, 0.5),
      'date_time' => Tomlrb::LocalDateTime.new(1979, 5, 27, 7, 32, 0),
      'offset' => Time.new(1979, 5, 27, 0, 32, 0, '-07:00')
    }

    dumped = Tomlrb.dump(config)
    _(dumped).must_include 'date = 1979-05-27'
    _(dumped).must_include 'time = 07:32:00.5'
    _(dumped).must_include 'date_time = 1979-05-27T07:32:00'
    _(dumped).must_include 'offset = 1979-05-27T00:32:00-07:00'
    _(roundtrip(config)).must_equal config
  end

  it 'preserves empty tables and arrays' do
    config = {
      'empty_table' => {},
      'empty_array' => [],
      'nested' => {'deep' => {}},
      'with_value' => {'kept' => 1, 'hole' => {}}
    }

    dumped = Tomlrb.dump(config)
    _(dumped).must_include '[empty_table]'
    _(dumped).must_include 'empty_array = []'
    _(dumped).must_include '[nested.deep]'
    _(dumped).must_include '[with_value.hole]'
    _(roundtrip(config)).must_equal config
  end

  it 'roundtrips mixed arrays inline' do
    config = {'mixed' => [1, 'two', true, 3.5, [1, 2], {'k' => 'v'}],
              'empty' => []}

    _(roundtrip(config)).must_equal config
  end

  it 'roundtrips arrays of tables, including empty instances' do
    config = {
      'products' => [
        {'name' => 'Hammer', 'sku' => 738594937},
        {},
        {'name' => 'Nail', 'sku' => 284758393, 'color' => 'gray'}
      ]
    }

    _(roundtrip(config)).must_equal config
  end

  it 'roundtrips multi-level arrays of tables with correct instance scoping' do
    config = {
      'fruit' => [
        {'name' => 'apple',
         'physical' => {'color' => 'red', 'shape' => 'round'},
         'variety' => [{'name' => 'red delicious'}, {'name' => 'granny smith'}]},
        {'name' => 'banana',
         'variety' => [{'name' => 'plantain'}]}
      ]
    }

    dumped = Tomlrb.dump(config)
    _(dumped.index('[[fruit]]')).must_equal 0
    _(dumped).must_include "[fruit.physical]\ncolor"
    banana_header = dumped.rindex("[[fruit]]")
    _(dumped.index('plantain')).must_be :>, banana_header
    _(dumped.index('red delicious')).must_be :<, banana_header
    _(roundtrip(config)).must_equal config
  end

  it 'keeps pair values on their array-of-tables instance even when inserted after a subtable' do
    config = {
      'servers' => [
        {'alpha' => {'ip' => '10.0.0.1'}, 'name' => 'first'},
        {'name' => 'second', 'beta' => {'ip' => '10.0.0.2'}}
      ]
    }

    dumped = Tomlrb.dump(config)
    first_instance = dumped.split('[[servers]]')[1]
    _(first_instance).must_include 'name = "first"'
    _(first_instance).wont_include 'second'
    _(roundtrip(config)).must_equal config
  end

  it 'treats dotted, quoted and whitespace keys as literal keys' do
    config = {
      'a.b' => {'c.d' => 1},
      'has space' => {'q "q"' => 2},
      'plain' => 3
    }

    dumped = Tomlrb.dump(config)
    _(dumped).must_include '["a.b"]'
    _(dumped).must_include '"c.d" = 1'
    _(dumped).must_include '["has space"]'
    _(roundtrip(config)).must_equal config
  end

  it 'escapes control characters and preserves unicode strings' do
    config = {'escapes' => "\t\n\r\"\b\\", 'ctrl' => "\x01\x7F", 'unicode' => '中国'}

    _(roundtrip(config)).must_equal config
  end

  it 'roundtrips hash values reused in multiple places' do
    shared = {'k' => 1}
    config = {'a' => shared, 'b' => {'c' => shared}}

    _(roundtrip(config)).must_equal config
  end

  it 'reports the structure path for a nil value' do
    error = _{ Tomlrb.dump('a' => {'b' => nil}) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'a.b'

    error = _{ Tomlrb.dump('servers' => [{'opts' => {'timeout' => nil}}]) }
              .must_raise Tomlrb::DumpError
    _(error.message).must_include 'servers[0].opts.timeout'

    error = _{ Tomlrb.dump('a' => [1, nil]) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'a[1]'
  end

  it 'reports the structure path for an unrepresentable value' do
    error = _{ Tomlrb.dump('a' => {'range' => (1..3)}) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'a.range'

    error = _{ Tomlrb.dump('a' => :sym) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'a'
  end

  it 'reports non-string keys with their parent path' do
    error = _{ Tomlrb.dump('a' => {1 => 2}) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'a'
    _(error.message).must_include 'keys must be Strings'
  end

  it 'detects a self-referential hash' do
    config = {'a' => {}}
    config['a']['back'] = config
    error = _{ Tomlrb.dump(config) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'circular'
  end

  it 'detects a self-referential array of tables' do
    items = []
    items << {'name' => 'first', 'next' => items}
    error = _{ Tomlrb.dump('items' => items) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'items[0].next'
  end

  it 'rejects a non-hash root' do
    _{ Tomlrb.dump([1, 2, 3]) }.must_raise Tomlrb::DumpError
    _{ Tomlrb.dump('scalar') }.must_raise Tomlrb::DumpError
  end

  it 'rejects strings that are not valid UTF-8' do
    bad = "\xFF".dup.force_encoding('UTF-8')
    error = _{ Tomlrb.dump('a' => bad) }.must_raise Tomlrb::DumpError
    _(error.message).must_include 'UTF-8'
  end
end
