# frozen_string_literal: true
require_relative "spec_helper"

Sequel.extension :shard_merge

describe "shard_merge extension" do
  # Mock paginated row source recording how many pages were read and how
  # many times it was closed.  Can be configured to raise on a specific
  # page read.
  class MockShardSource
    attr_reader :read_count, :close_count

    def initialize(pages, fail_on_read=nil)
      @pages = pages
      @fail_on_read = fail_on_read
      @read_count = 0
      @close_count = 0
    end

    def next_row
      if @buffer.nil? || @buffer.empty?
        return nil if @pages.empty?
        @read_count += 1
        raise Sequel::DatabaseError, "shard read failure" if @fail_on_read == @read_count
        @buffer = @pages.shift
      end
      @buffer.shift
    end

    def close
      @close_count += 1
      self
    end
  end

  before do
    @keys = Sequel::ShardMerge.order_keys([Sequel.desc(:ts), :id], :id)
  end

  def reader_for(sources, limit=nil)
    Sequel::ShardMerge::Reader.new(sources, @keys, limit)
  end

  it "should merge rows from all shards in global order, handling duplicates across shards" do
    sources = [
      MockShardSource.new([[{:id=>1, :ts=>5}, {:id=>2, :ts=>3}], [{:id=>5, :ts=>1}]]),
      MockShardSource.new([[{:id=>3, :ts=>3}], [{:id=>4, :ts=>2}]]),
      MockShardSource.new([[{:id=>6, :ts=>3}]])
    ]
    reader_for(sources).map{|r| [r[:ts], r[:id]]}.must_equal [[5, 1], [3, 2], [3, 3], [3, 6], [2, 4], [1, 5]]
    sources.map{|s| s.read_count}.must_equal [2, 2, 1]
  end

  it "should handle empty shards" do
    sources = [
      MockShardSource.new([]),
      MockShardSource.new([[{:id=>1, :ts=>2}]]),
      MockShardSource.new([])
    ]
    reader_for(sources).map{|r| r[:id]}.must_equal [1]
    sources.map{|s| s.read_count}.must_equal [0, 1, 0]
  end

  it "should not read from any shard until the reader is iterated" do
    sources = [MockShardSource.new([[{:id=>1, :ts=>1}]]), MockShardSource.new([[{:id=>2, :ts=>2}]])]
    reader = reader_for(sources)
    sources.map{|s| s.read_count}.must_equal [0, 0]
    reader.closed?.must_equal false
    reader.to_a
  end

  it "should only read pages as needed to satisfy the limit" do
    pages = [[{:id=>1, :ts=>10}, {:id=>2, :ts=>9}], [{:id=>3, :ts=>8}, {:id=>4, :ts=>7}], [{:id=>5, :ts=>6}]]
    sources = [MockShardSource.new(pages.map{|p| p.dup}), MockShardSource.new(pages.map{|p| p.dup})]
    reader_for(sources, 3).map{|r| r[:id]}.must_equal [1, 1, 2]
    # Each shard is primed with a single page, which is enough to satisfy
    # the limit; the remaining pages are never read.
    sources.map{|s| s.read_count}.must_equal [1, 1]
  end

  it "should close all shard cursors when iteration completes" do
    sources = [MockShardSource.new([[{:id=>1, :ts=>1}]]), MockShardSource.new([[{:id=>2, :ts=>2}]])]
    reader = reader_for(sources)
    reader.each{|r| }
    sources.map{|s| s.close_count}.must_equal [1, 1]
    reader.closed?.must_equal true
    proc{reader.each{|r| }}.must_raise(Sequel::Error)
  end

  it "should close all shard cursors when the caller stops early" do
    sources = [
      MockShardSource.new([[{:id=>1, :ts=>3}], [{:id=>2, :ts=>2}], [{:id=>3, :ts=>1}]]),
      MockShardSource.new([[{:id=>4, :ts=>3}], [{:id=>5, :ts=>2}]])
    ]
    reader = reader_for(sources)
    seen = []
    reader.each do |row|
      seen << row[:id]
      break
    end
    seen.must_equal [1]
    sources.map{|s| s.close_count}.must_equal [1, 1]
    sources.map{|s| s.read_count}.must_equal [1, 1]
    reader.closed?.must_equal true
  end

  it "should close all shard cursors when using Enumerable#take" do
    sources = [MockShardSource.new([[{:id=>1, :ts=>2}], [{:id=>3, :ts=>1}]]), MockShardSource.new([[{:id=>2, :ts=>2}]])]
    reader_for(sources).take(2).map{|r| r[:id]}.must_equal [1, 2]
    sources.map{|s| s.close_count}.must_equal [1, 1]
  end

  it "should propagate read errors and close all shard cursors" do
    sources = [
      MockShardSource.new([[{:id=>1, :ts=>3}], [{:id=>2, :ts=>2}]], 2),
      MockShardSource.new([[{:id=>3, :ts=>4}]])
    ]
    reader = reader_for(sources)
    proc{reader.to_a}.must_raise(Sequel::DatabaseError)
    sources.map{|s| s.close_count}.must_equal [1, 1]
    reader.closed?.must_equal true
  end

  it "should make explicit close idempotent" do
    sources = [MockShardSource.new([[{:id=>1, :ts=>1}]])]
    reader = reader_for(sources)
    reader.close
    reader.close
    sources.map{|s| s.close_count}.must_equal [1]
    reader.closed?.must_equal true
    proc{reader.each{|r| }}.must_raise(Sequel::Error)
  end

  it "should validate order and unique options" do
    db = Sequel.mock(:servers=>{:s1=>{}}).extension(:shard_merge)
    proc{db.shard_merge([], :events, :order=>[:id], :unique=>:id)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :unique=>:id)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[], :unique=>:id)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[:ts, :id])}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[:id], :unique=>:ts)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[:ts, Sequel.desc(:id)], :unique=>:ts)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[Sequel.function(:now), :id], :unique=>:id)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[:id], :unique=>:id, :limit=>0)}.must_raise(Sequel::Error)
    proc{db.shard_merge([:s1], :events, :order=>[:id], :unique=>:id, :page_size=>0)}.must_raise(Sequel::Error)
  end

  it "should query each shard with limit/offset pages and merge in global order" do
    shard_rows = {
      "s1"=>[{:id=>1, :ts=>5}, {:id=>2, :ts=>3}, {:id=>5, :ts=>1}],
      "s2"=>[{:id=>3, :ts=>3}],
      "s3"=>[]
    }
    reads = Hash.new(0)
    fetch = proc do |sql|
      shard = sql[/ -- (\w+)\z/, 1]
      reads[shard] += 1
      offset = sql[/OFFSET (\d+)/, 1].to_i
      limit = sql[/LIMIT (\d+)/, 1].to_i
      shard_rows[shard][offset, limit] || []
    end
    db = Sequel.mock(:servers=>{:s1=>{}, :s2=>{}, :s3=>{}}, :fetch=>fetch).extension(:shard_merge)

    reader = db.shard_merge([:s1, :s2, :s3], :events, :order=>[Sequel.desc(:ts), :id], :unique=>:id, :page_size=>2)
    db.sqls.must_equal []
    reader.map{|r| [r[:ts], r[:id]]}.must_equal [[5, 1], [3, 2], [3, 3], [1, 5]]
    db.sqls.must_equal [
      "SELECT * FROM events ORDER BY ts DESC, id LIMIT 2 OFFSET 0 -- s1",
      "SELECT * FROM events ORDER BY ts DESC, id LIMIT 2 OFFSET 0 -- s2",
      "SELECT * FROM events ORDER BY ts DESC, id LIMIT 2 OFFSET 0 -- s3",
      "SELECT * FROM events ORDER BY ts DESC, id LIMIT 2 OFFSET 2 -- s1"
    ]
    reads.must_equal("s1"=>2, "s2"=>1, "s3"=>1)
  end

  it "should stop querying shards once the limit is reached" do
    shard_rows = {
      "s1"=>[{:id=>1, :ts=>5}, {:id=>2, :ts=>4}, {:id=>3, :ts=>3}],
      "s2"=>[{:id=>4, :ts=>2}, {:id=>5, :ts=>1}]
    }
    reads = Hash.new(0)
    fetch = proc do |sql|
      shard = sql[/ -- (\w+)\z/, 1]
      reads[shard] += 1
      offset = sql[/OFFSET (\d+)/, 1].to_i
      limit = sql[/LIMIT (\d+)/, 1].to_i
      shard_rows[shard][offset, limit] || []
    end
    db = Sequel.mock(:servers=>{:s1=>{}, :s2=>{}}, :fetch=>fetch).extension(:shard_merge)

    reader = db.shard_merge([:s1, :s2], :events, :order=>[Sequel.desc(:ts), :id], :unique=>:id, :limit=>3, :page_size=>2)
    reader.map{|r| r[:id]}.must_equal [1, 2, 3]
    # s2 only needs its first page; s1 needs its second page to supply
    # the third row, and no shard is read past that.
    reads.must_equal("s1"=>2, "s2"=>1)
  end

  it "should support passing a dataset instead of a table name" do
    db = Sequel.mock(:servers=>{:s1=>{}}, :fetch=>[{:id=>1, :ts=>1}]).extension(:shard_merge)
    ds = db[:events].where(:kind=>"read")
    reader = db.shard_merge([:s1], ds, :order=>[:ts, :id], :unique=>:id, :page_size=>10)
    reader.map{|r| r[:id]}.must_equal [1]
    db.sqls.must_equal ["SELECT * FROM events WHERE (kind = 'read') ORDER BY ts, id LIMIT 10 OFFSET 0 -- s1"]
  end
end
