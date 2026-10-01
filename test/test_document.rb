require 'minitest_helper'

describe Tomlrb::Document do
  def load_document
    Tomlrb::Document.load_file('./test/document_example.toml')
  end

  def expected_edited_source
    <<~TOML
      # 网关服务配置 —— 手工维护，请勿重排
      title = "gateway"      # 服务名
      version = 1

      [server]
      host = '0.0.0.0'       # 监听地址
      port = 0x1F91          # 8080，保持十六进制
      sankey = "keep.me"     # 哨兵：不得被改写

      [database]
      net.ipv4 = "10.0.0.1"  # 点分键
      timeout  =  30         # 秒
      dsn = "postgres://db.internal:5432/app"
      pool    =    10
      options = { retries = 3, ssl = true }  # 内联表，保持单行
      ports = [
        5432,
        5433, # 只读副本
      ]

      [[servers]]
      name = "web-1"
      weight = 1_000         # 下划线分隔

      [[servers]]
      role = "primary"
      name = "web-2"         # 备用节点
      weight = 2500
    TOML
  end

  def expected_edited_hash
    {
      'title' => 'gateway',
      'version' => 1,
      'server' => {
        'host' => '0.0.0.0',
        'port' => 0x1F91,
        'sankey' => 'keep.me'
      },
      'database' => {
        'timeout' => 30,
        'net' => { 'ipv4' => '10.0.0.1' },
        'dsn' => 'postgres://db.internal:5432/app',
        'pool' => 10,
        'options' => { 'retries' => 3, 'ssl' => true },
        'ports' => [5432, 5433]
      },
      'servers' => [
        { 'name' => 'web-1', 'weight' => 1000 },
        { 'role' => 'primary', 'name' => 'web-2', 'weight' => 2500 }
      ]
    }
  end

  def apply_consecutive_edits(doc)
    doc.set('database.pool', 10)
    doc.set('servers[1].weight', 2500)
    doc.rename('database.url', 'dsn')
    doc.move('server.timeout', 'database')
    doc.move('server.net.ipv4', 'database')
    doc.set('server.port', Tomlrb::Document::Raw.new('0x1F91'))
    doc.move(['servers', 0, 'role'], ['servers', 1])
    doc
  end

  it 'applies consecutive edits while preserving untouched bytes' do
    doc = apply_consecutive_edits(load_document)
    _(doc.to_s).must_equal expected_edited_source
  end

  it 're-parses to the expected structure after consecutive edits' do
    doc = apply_consecutive_edits(load_document)
    _(Tomlrb.parse(doc.to_s)).must_equal expected_edited_hash
    _(doc.to_h).must_equal expected_edited_hash
  end

  it 'keeps sentinel lines byte-identical' do
    doc = apply_consecutive_edits(load_document)
    _(doc.to_s).must_include %(sankey = "keep.me"     # 哨兵：不得被改写)
    _(doc.to_s).must_include 'weight = 1_000         # 下划线分隔'
    _(doc.to_s).must_include 'options = { retries = 3, ssl = true }  # 内联表，保持单行'
    _(doc.to_s).must_include "host = '0.0.0.0'       # 监听地址"
  end
end

describe Tomlrb::Document do
  def load_document
    Tomlrb::Document.load_file('./test/document_example.toml')
  end

  it 'rejects a rename that would duplicate an existing key' do
    doc = load_document
    before = doc.to_s
    _ { doc.rename('database.pool', 'url') }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects a move that would duplicate an existing key' do
    doc = load_document
    before = doc.to_s
    _ { doc.move('server.sankey', 'database', as: 'pool') }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects a move into a non-table value' do
    doc = load_document
    before = doc.to_s
    _ { doc.move('server.sankey', 'database.pool') }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects a move into an inline table' do
    doc = load_document
    before = doc.to_s
    _ { doc.move('server.sankey', 'database.options') }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects a move into a missing array-of-tables instance' do
    doc = load_document
    before = doc.to_s
    _ { doc.move('server.sankey', ['servers', 5]) }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects setting a missing key or a whole table' do
    doc = load_document
    before = doc.to_s
    _ { doc.set('database.nope', 1) }.must_raise(Tomlrb::Document::EditError)
    _ { doc.set('database', 1) }.must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'rejects edits that fail to re-parse and stays unchanged' do
    doc = load_document
    before = doc.to_s
    _ { doc.set('server.port', Tomlrb::Document::Raw.new('not toml!!')) }
      .must_raise(Tomlrb::Document::EditError)
    _(doc.to_s).must_equal before
  end

  it 'keeps the document editable after a rejected edit' do
    doc = load_document
    _ { doc.rename('database.pool', 'url') }.must_raise(Tomlrb::Document::EditError)
    doc.set('database.pool', 42)
    _(doc.to_h['database']['pool']).must_equal 42
  end

  it 'moves a key into a new table appended at the end' do
    doc = load_document
    doc.move('server.sankey', 'backup.primary')
    _(doc.to_s.end_with?(%(sankey = "keep.me"     # 哨兵：不得被改写\n))).must_equal true
    _(doc.to_s).must_include "[backup.primary]\n"
    _(doc.to_h['backup']).must_equal('primary' => { 'sankey' => 'keep.me' })
    _(doc.to_h['server']).wont_include 'sankey'
  end

  it 'moves multi-line values together with their inner comments' do
    doc = load_document
    doc.move('database.ports', 'server')
    _(doc.to_s).must_include "[server]\nports = [\n  5432,\n  5433, # 只读副本\n]\n"
    _(doc.to_h['server']['ports']).must_equal [5432, 5433]
    _(doc.to_h['database']).wont_include 'ports'
  end

  it 'renames quoted keys and addresses them in paths' do
    doc = load_document
    doc.set('server."sankey"', 'changed')
    _(doc.to_h['server']['sankey']).must_equal 'changed'
    doc.rename('server.host', 'listen addr')
    _(doc.to_s).must_include %("listen addr" = '0.0.0.0'       # 监听地址)
    _(doc.to_h['server']['listen addr']).must_equal '0.0.0.0'
  end

  it 'serializes ruby values' do
    doc = Tomlrb::Document.parse("[t]\na = 1\nb = 1\nc = 1\nd = 1\ne = 1\nf = 1\ng = 1\n")
    doc.set('t.a', "he said \"hi\"\n")
    doc.set('t.b', [1, 'two', true])
    doc.set('t.c', { 'x' => 1.5 })
    doc.set('t.d', 2.5)
    doc.set('t.e', false)
    doc.set('t.f', Tomlrb::Document::Raw.new('0xFF'))
    doc.set('t.g', Float::INFINITY)
    _(doc.to_s).must_equal <<~TOML
      [t]
      a = "he said \\"hi\\"\\n"
      b = [1, "two", true]
      c = {x = 1.5}
      d = 2.5
      e = false
      f = 0xFF
      g = inf
    TOML
    _(doc.to_h['t']['f']).must_equal 255
  end

  it 'raises on invalid documents' do
    _ { Tomlrb::Document.parse("[unclosed\na = 1") }.must_raise(Tomlrb::ParseError)
  end
end
