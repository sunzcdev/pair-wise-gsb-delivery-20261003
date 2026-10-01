require 'minitest_helper'

FIXTURE = File.expand_path('../editable_example.toml', __FILE__)

def fixture_text
  File.read(FIXTURE, encoding: 'utf-8')
end

describe Tomlrb::Document do
  def build
    Tomlrb::Document.new(fixture_text)
  end

  it "parses the fixture back to the same structure" do
    document = build
    _(document.to_ruby).must_equal Tomlrb.parse(fixture_text)
  end

  it "changes a scalar value while keeping the rest byte-for-byte" do
    document = build
    original = document.text
    value_range = document.instance_variable_get(:@model).entries
                    .find { |entry| entry.path == ['title'] }.value_site[1]

    document.set('title', 'new-title')

    _(document.to_ruby['title']).must_equal 'new-title'
    _(document.untouched_bytes_preserved?([value_range])).must_equal true
    _(document.text).must_include '# Mixed configuration for the document editor tests.'
    _(document.text).must_include 'retries = 0x0A        # hex keeps its spelling'
    _(document.text).must_include 'ratio = 3.140'
  end

  it "keeps hex and float spelling on unrelated lines" do
    document = build
    document.set(['server', 'port'], 9090)
    _(document.text).must_include 'retries = 0x0A        # hex keeps its spelling'
    _(document.text).must_include 'ratio = 3.140'
    _(document.text).must_include 'weight = 1.0'
  end

  it "renames a plain key" do
    document = build
    document.rename(['server', 'port'], 'listen')
    result = document.to_ruby
    _(result['server'].key?('listen')).must_equal true
    _(result['server'].key?('port')).must_equal false
    _(document.text).must_include 'listen = 8080'
    _(document.text).wont_include 'port = 8080'
  end

  it "targets a specific array-of-tables instance" do
    document = build
    document.set(['targets', 1, 'name'], 'BETA')
    targets = document.to_ruby['targets']
    _(targets[0]['name']).must_equal 'alpha'
    _(targets[1]['name']).must_equal 'BETA'
    _(targets[2]['name']).must_equal 'gamma'
  end

  it "moves a value across sections and removes the source line" do
    document = build
    document.move(['pool', 'size'], ['server', 'workers'])
    result = document.to_ruby
    _(result['server']['workers']).must_equal 4
    _(result['pool'].key?('size')).must_equal false
    _(document.text.lines.grep(/workers = 4/).length).must_equal 1
  end

  it "moves a key out of an inline table cleanly" do
    document = build
    document.move(['server', 'meta', 'weight'], ['server', 'meta_weight'])
    result = document.to_ruby
    _(result['server']['meta'].key?('weight')).must_equal false
    _(result['server']['meta_weight']).must_equal 1.0
    _(document.text).must_include 'meta = { region = "cn", flags = ["a", \'b\'] }'
  end

  it "moves the last inline pair leaving an empty table" do
    text = 't = { a = 1 }' + "\n"
    document = Tomlrb::Document.new(text)
    document.move(['t', 'a'], ['a'])
    _(document.to_ruby).must_equal('t' => {}, 'a' => 1)
    _(document.text).must_include 't = {}'
  end

  it "renames a table header and its sub-table headers only" do
    document = build
    document.rename(['server'], 'service')
    result = document.to_ruby
    _(result.key?('service')).must_equal true
    _(result.key?('server')).must_equal false
    _(result['service']['host']).must_equal '127.0.0.1'
    _(result['service']['tls']['enabled']).must_equal true
    _(document.text).must_include '[service]'
    _(document.text).must_include '[service.tls]'
    _(document.text).wont_include '[server'
  end

  it "renames an array-of-tables name across every instance header" do
    text = File.read(File.expand_path('../nested_array_of_tables.toml', __FILE__), encoding: 'utf-8')
    document = Tomlrb::Document.new(text)
    document.rename(['fruit'], 'produce')
    result = document.to_ruby
    _(result.key?('fruit')).must_equal false
    _(result['produce'].length).must_equal 2
    _(result['produce'][0]['variety'].length).must_equal 2
    _(document.text.lines.count { |line| line =~ /^\s*\[\[?produce\b/ }).must_equal 7
    _(document.text.lines.count { |line| line =~ /fruit/ }).must_equal 0
  end

  it "renames an instance-scoped leaf" do
    document = build
    document.rename(['targets', 0, 'weight'], 'priority')
    result = document.to_ruby
    _(result['targets'][0].key?('priority')).must_equal true
    _(result['targets'][0].key?('weight')).must_equal false
    _(result['targets'][1]['weight']).must_equal 20
  end

  it "adds a key into an existing inline table" do
    document = build
    document.set(['server', 'meta', 'priority'], 'high')
    _(document.to_ruby['server']['meta']['priority']).must_equal 'high'
    _(document.text).must_include "flags = [\"a\", 'b'], priority = \"high\" }"
  end

  it "moves a value into a chosen array-of-tables instance only" do
    document = build
    document.move(['ratio'], ['targets', 0, 'ratio'])
    result = document.to_ruby
    _(result['targets'][0]['ratio']).must_equal 3.14
    _(result['targets'][1].key?('ratio')).must_equal false
    _(result['targets'][2].key?('ratio')).must_equal false
    _(result.key?('ratio')).must_equal false
  end

  it "moves into a brand new table when no headers exist yet" do
    document = Tomlrb::Document.new("a = 1\n")
    document.move(['a'], ['brand', 'new', 'a'])
    _(document.to_ruby).must_equal('brand' => { 'new' => { 'a' => 1 } })
  end

  it "keeps document valid after a later rejected edit" do
    document = build
    document.rename(['server', 'port'], 'listen')
    committed = document.text
    _{ document.rename(['server', 'host'], 'listen') }.must_raise Tomlrb::EditError
    _(document.text).must_equal committed
    _(document.to_ruby['server'].keys).must_include 'listen'
  end

  it "moves into a dotted-key-defined table without changing neighbours" do
    text = "[a]\nx = 1\ndeeper.y = 2\n\n[b]\nz = 3\n"
    document = Tomlrb::Document.new(text)
    document.move(['b', 'z'], ['a', 'deeper', 'z'])
    result = document.to_ruby
    _(result['a']['x']).must_equal 1
    _(result['a']['deeper']['y']).must_equal 2
    _(result['a']['deeper']['z']).must_equal 3
    _(result['b']).must_equal({})
  end

  it "does not rewrite sibling leaf keys when renaming a dotted sub-table" do
    text = "[a]\nx = 1\ndeeper.y = 2\n"
    document = Tomlrb::Document.new(text)
    document.rename(['a', 'deeper'], 'lower')
    result = document.to_ruby
    _(result['a']['x']).must_equal 1
    _(result['a']['lower']['y']).must_equal 2
    _(result['a'].key?('deeper')).must_equal false
  end

  it "rejects duplicate declarations without touching the document" do
    document = build
    before = document.text
    _{ document.rename(['server', 'host'], 'port') }.must_raise Tomlrb::EditError
    _(document.text).must_equal before
    _(document.to_ruby).must_equal Tomlrb.parse(before)
  end

  it "rejects moving onto an existing path atomically" do
    document = build
    before = document.text
    _{ document.move(['title'], ['server', 'host']) }.must_raise Tomlrb::EditError
    _(document.text).must_equal before
  end

  it "rejects replacing a table with a scalar" do
    document = build
    _{ document.set(['server'], 1) }.must_raise Tomlrb::EditError
  end

  it "rejects renaming a single array-of-tables instance" do
    document = build
    _{ document.rename(['targets', 1], 'x') }.must_raise Tomlrb::EditError
  end

  it "rejects editing a missing parent table" do
    document = build
    _{ document.set(['nope', 'x'], 1) }.must_raise Tomlrb::EditError
  end

  it "applies a chain of edits, re-parses and preserves untouched bytes" do
    document = build
    tracked = []
    tracked << document.instance_variable_get(:@model).entries
                  .find { |entry| entry.path == ['title'] }.value_site[1]

    document.set('title', 'new-title')
    document.rename(['server', 'port'], 'listen')
    document.move(['pool', 'size'], ['server', 'workers'])
    document.set(['targets', 1, 'name'], 'BETA')
    document.rename(['extra', 'dotted'], 'spaced')

    result = document.to_ruby
    _(result['title']).must_equal 'new-title'
    _(result['server']['listen']).must_equal 8080
    _(result['server']['workers']).must_equal 4
    _(result['server'].key?('port')).must_equal false
    _(result['pool'].key?('size')).must_equal false
    _(result['targets'][1]['name']).must_equal 'BETA'
    _(result['extra']['spaced']['only']).must_equal 'once'

    changed = document.changed_original_ranges.map { |from, to| (from...to) }
    _(document.untouched_bytes_preserved?(changed)).must_equal true
  end

  it "keeps the dotted-key sibling untouched when renaming the sub-table" do
    document = build
    document.rename(['extra', 'dotted'], 'spaced')
    _(document.text).must_include 'spaced.only = "once"'
    _(document.to_ruby['extra']).must_equal('spaced' => { 'only' => 'once' })
  end
end
