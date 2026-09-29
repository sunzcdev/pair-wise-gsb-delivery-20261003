# frozen_string_literal: true
require_relative "spec_helper"

Sequel.extension :shard_merge

# Mock paged source recording read and close counts.  Pages are arrays of
# row hashes; an Exception entry is raised when reached.
class ShardMergeMockSource
  attr_reader :read_count, :close_count

  def initialize(pages)
    @pages = pages.dup
    @read_count = 0
    @close_count = 0
  end

  def next_page
    @read_count += 1
    page = @pages.shift
    raise page.first if page.is_a?(Array) && page.first.is_a?(Exception)
    page || []
  end

  def close
    @close_count += 1
  end

  def closed?
    @close_count > 0
  end
end

describe "Sequel::ShardMerge::Merger" do
  before do
    @order = [[:v, false], [:id, false]]
  end

  def merger(sources, limit=nil)
    Sequel::ShardMerge::Merger.new(sources, @order, limit)
  end

  it "should merge rows from all shards in global order" do
    s1 = ShardMergeMockSource.new([[{v: 1, id: 1}, {v: 3, id: 5}], [{v: 5, id: 9}]])
    s2 = ShardMergeMockSource.new([[{v: 1, id: 2}, {v: 2, id: 4}], [{v: 4, id: 7}, {v: 5, id: 8}]])
    s3 = ShardMergeMockSource.new([[{v: 2, id: 3}, {v: 4, id: 6}]])
    merger([s1, s2, s3]).map{|r| [r[:v], r[:id]]}.must_equal [[1, 1], [1, 2], [2, 3], [2, 4], [3, 5], [4, 6], [4, 7], [5, 8], [5, 9]]
  end

  it "should handle descending order columns" do
    order = [[:v, true], [:id, false]]
    s1 = ShardMergeMockSource.new([[{v: 5, id: 1}, {v: 3, id: 3}]])
    s2 = ShardMergeMockSource.new([[{v: 4, id: 2}, {v: 3, id: 1}]])
    Sequel::ShardMerge::Merger.new([s1, s2], order).map{|r| [r[:v], r[:id]]}.
      must_equal [[5, 1], [4, 2], [3, 1], [3, 3]]
  end

  it "should handle empty shards" do
    s1 = ShardMergeMockSource.new([[]])
    s2 = ShardMergeMockSource.new([[{v: 1, id: 1}]])
    s3 = ShardMergeMockSource.new([])
    merger([s1, s2, s3]).map{|r| r[:id]}.must_equal [1]
    s1.read_count.must_equal 1
    s3.read_count.must_equal 1
  end

  it "should return nothing when all shards are empty" do
    merger([ShardMergeMockSource.new([]), ShardMergeMockSource.new([])]).to_a.must_equal []
  end

  it "should return duplicate rows appearing on multiple shards, in deterministic shard order" do
    s1 = ShardMergeMockSource.new([[{v: 1, id: 1, shard: :s1}]])
    s2 = ShardMergeMockSource.new([[{v: 1, id: 1, shard: :s2}, {v: 1, id: 1, shard: :s2b}]])
    merger([s1, s2]).map{|r| r[:shard]}.must_equal [:s1, :s2, :s2b]
  end

  it "should respect the limit and not read more pages than needed" do
    s1 = ShardMergeMockSource.new([[{v: 1, id: 1}], [{v: 2, id: 2}], [{v: 3, id: 3}]])
    s2 = ShardMergeMockSource.new([[{v: 4, id: 4}], [{v: 5, id: 5}]])
    merger([s1, s2], 2).map{|r| r[:id]}.must_equal [1, 2]
    s1.read_count.must_equal 2
    s2.read_count.must_equal 1
  end

  it "should support a limit of 0 without reading any pages" do
    s1 = ShardMergeMockSource.new([[{v: 1, id: 1}]])
    merger([s1], 0).to_a.must_equal []
    s1.read_count.must_equal 0
    s1.close_count.must_equal 1
  end

  it "should close all sources after full consumption" do
    sources = [ShardMergeMockSource.new([[{v: 1, id: 1}]]), ShardMergeMockSource.new([[{v: 2, id: 2}]])]
    merger(sources).to_a
    sources.each{|s| s.close_count.must_equal 1}
  end

  it "should close all sources when the caller stops iteration early with break" do
    sources = [ShardMergeMockSource.new([[{v: 1, id: 1}], [{v: 3, id: 3}]]), ShardMergeMockSource.new([[{v: 2, id: 2}]])]
    yielded = []
    merger(sources).each do |row|
      yielded << row[:id]
      break
    end
    yielded.must_equal [1]
    sources.each{|s| s.close_count.must_equal 1}
  end

  it "should close all sources when using Enumerable#take" do
    sources = [ShardMergeMockSource.new([[{v: 1, id: 1}], [{v: 3, id: 3}]]), ShardMergeMockSource.new([[{v: 2, id: 2}]])]
    merger(sources).take(1).map{|r| r[:id]}.must_equal [1]
    sources.each{|s| s.close_count.must_equal 1}
  end

  it "should close all sources when the block raises" do
    sources = [ShardMergeMockSource.new([[{v: 1, id: 1}]]), ShardMergeMockSource.new([[{v: 2, id: 2}]])]
    proc{merger(sources).each{raise ArgumentError, "caller error"}}.must_raise(ArgumentError)
    sources.each{|s| s.close_count.must_equal 1}
  end

  it "should propagate shard read errors and close all sources" do
    error = Sequel::DatabaseError.new("shard read failure")
    s1 = ShardMergeMockSource.new([[{v: 1, id: 1}], [error]])
    s2 = ShardMergeMockSource.new([[{v: 2, id: 2}]])
    proc{merger([s1, s2]).to_a}.must_raise(Sequel::DatabaseError)
    s1.close_count.must_equal 1
    s2.close_count.must_equal 1
  end

  it "should propagate errors raised on the first page read and close all sources" do
    error = Sequel::DatabaseError.new("shard unavailable")
    s1 = ShardMergeMockSource.new([[error]])
    s2 = ShardMergeMockSource.new([[{v: 2, id: 2}]])
    proc{merger([s1, s2]).to_a}.must_raise(Sequel::DatabaseError)
    s1.close_count.must_equal 1
    s2.close_count.must_equal 1
  end
end

describe "Sequel::ShardMerge.normalize_order" do
  it "should require an order" do
    proc{Sequel::ShardMerge.normalize_order(nil)}.must_raise(Sequel::ShardMerge::Error)
    proc{Sequel::ShardMerge.normalize_order([])}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should reject non-column order entries" do
    proc{Sequel::ShardMerge.normalize_order([Sequel.function(:lower, :name), :id])}.must_raise(Sequel::ShardMerge::Error)
    proc{Sequel::ShardMerge.normalize_order([Sequel.desc(Sequel.function(:lower, :name)), :id])}.must_raise(Sequel::ShardMerge::Error)
    proc{Sequel::ShardMerge.normalize_order(["name", :id])}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should reject duplicate order columns" do
    proc{Sequel::ShardMerge.normalize_order([:v, Sequel.desc(:v)])}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should reject explicit NULLS FIRST/LAST ordering" do
    proc{Sequel::ShardMerge.normalize_order([Sequel.desc(:v, nulls: :last), :id])}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should normalize symbols and Sequel.asc/Sequel.desc" do
    Sequel::ShardMerge.normalize_order([:v, Sequel.desc(:w), Sequel.asc(:id)]).
      must_equal [[:v, false], [:w, true], [:id, false]]
  end
end

describe "Dataset#shard_merge" do
  before do
    @pages = {
      s1: [[{id: 1, v: 10}, {id: 3, v: 30}], [{id: 5, v: 50}]],
      s2: [[{id: 2, v: 10}, {id: 4, v: 40}]],
      s3: [],
    }
    @reads = Hash.new(0)
    pages = @pages
    reads = @reads
    @db = Sequel.mock(servers: {s1: {}, s2: {}, s3: {}}, fetch: proc do |sql|
      server = sql[/ -- (\w+)\z/, 1].to_sym
      page = pages[server][reads[server]] || []
      reads[server] += 1
      page
    end)
    @db.extension(:shard_merge)
  end

  it "should merge rows from all shards in global order using keyset pagination" do
    rows = @db[:items].shard_merge([:s1, :s2, :s3], order: [:v, :id], page_size: 2).to_a
    rows.map{|r| r[:id]}.must_equal [1, 2, 3, 4, 5]
    @reads.must_equal(s1: 2, s2: 2, s3: 1)
    @db.sqls.must_equal [
      "SELECT * FROM items ORDER BY v ASC, id ASC LIMIT 2 -- s1",
      "SELECT * FROM items ORDER BY v ASC, id ASC LIMIT 2 -- s2",
      "SELECT * FROM items ORDER BY v ASC, id ASC LIMIT 2 -- s3",
      "SELECT * FROM items WHERE ((v > 30) OR ((v = 30) AND (id > 3))) ORDER BY v ASC, id ASC LIMIT 2 -- s1",
      "SELECT * FROM items WHERE ((v > 40) OR ((v = 40) AND (id > 4))) ORDER BY v ASC, id ASC LIMIT 2 -- s2",
    ]
  end

  it "should respect the limit and not read more pages than needed" do
    rows = @db[:items].shard_merge([:s1, :s2, :s3], order: [:v, :id], limit: 3, page_size: 2).to_a
    rows.map{|r| r[:id]}.must_equal [1, 2, 3]
    @reads.must_equal(s1: 1, s2: 1, s3: 1)
  end

  it "should support descending order columns" do
    @pages = {
      s1: [[{id: 5, v: 50}], [{id: 1, v: 10}]],
      s2: [[{id: 4, v: 40}], [{id: 2, v: 10}]],
      s3: [],
    }
    pages = @pages
    reads = @reads
    @db = Sequel.mock(servers: {s1: {}, s2: {}, s3: {}}, fetch: proc do |sql|
      server = sql[/ -- (\w+)\z/, 1].to_sym
      page = pages[server][reads[server]] || []
      reads[server] += 1
      page
    end)
    @db.extension(:shard_merge)
    rows = @db[:items].shard_merge([:s1, :s2, :s3], order: [Sequel.desc(:v), :id], page_size: 1).to_a
    rows.map{|r| r[:id]}.must_equal [5, 4, 1, 2]
    @db.sqls.must_equal [
      "SELECT * FROM items ORDER BY v DESC, id ASC LIMIT 1 -- s1",
      "SELECT * FROM items ORDER BY v DESC, id ASC LIMIT 1 -- s2",
      "SELECT * FROM items ORDER BY v DESC, id ASC LIMIT 1 -- s3",
      "SELECT * FROM items WHERE ((v < 50) OR ((v = 50) AND (id > 5))) ORDER BY v DESC, id ASC LIMIT 1 -- s1",
      "SELECT * FROM items WHERE ((v < 40) OR ((v = 40) AND (id > 4))) ORDER BY v DESC, id ASC LIMIT 1 -- s2",
      "SELECT * FROM items WHERE ((v < 10) OR ((v = 10) AND (id > 1))) ORDER BY v DESC, id ASC LIMIT 1 -- s1",
      "SELECT * FROM items WHERE ((v < 10) OR ((v = 10) AND (id > 2))) ORDER BY v DESC, id ASC LIMIT 1 -- s2",
    ]
  end

  it "should not issue any queries until the result is consumed" do
    @db[:items].shard_merge([:s1, :s2], order: [:v, :id], limit: 5)
    @db.sqls.must_equal []
  end

  it "should not issue any queries for a limit of 0" do
    @db[:items].shard_merge([:s1, :s2], order: [:v, :id], limit: 0).to_a.must_equal []
    @db.sqls.must_equal []
  end

  it "should raise for invalid order options" do
    proc{@db[:items].shard_merge([:s1], order: [])}.must_raise(Sequel::ShardMerge::Error)
    proc{@db[:items].shard_merge([:s1], order: [:v, :v])}.must_raise(Sequel::ShardMerge::Error)
    proc{@db[:items].shard_merge([:s1], order: [Sequel.function(:lower, :v), :id])}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should raise when reading a source after the merge has finished" do
    merge = @db[:items].shard_merge([:s1], order: [:v, :id], page_size: 2)
    merge.to_a
    source = merge.instance_variable_get(:@sources).first
    source.must_be :closed?
    proc{source.next_page}.must_raise(Sequel::ShardMerge::Error)
  end

  it "should propagate shard read errors and stop reading" do
    db = Sequel.mock(servers: {s1: {}, s2: {}}, fetch: proc{|sql| raise Sequel::DatabaseError, "shard down"})
    db.extension(:shard_merge)
    proc{db[:items].shard_merge([:s1, :s2], order: [:v, :id]).to_a}.must_raise(Sequel::DatabaseError)
    db.sqls.length.must_equal 1
  end
end
