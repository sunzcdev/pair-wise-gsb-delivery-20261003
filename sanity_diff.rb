# frozen_string_literal: true
require "sequel"

class << Sequel::Model
  attr_writer :db_schema
  alias orig_columns columns
  def columns(*cols)
    return super if cols.empty?
    define_method(:columns){cols}
    alias_method(:columns, :columns)
    @dataset.send(:columns=, cols) if @dataset
    def_column_accessor(*cols)
    @columns = cols
    @db_schema = {}
    cols.each{|c| @db_schema[c] = {}}
  end
end
Sequel::Model.use_transactions = false

DB = Sequel.mock(:autoid=>1, :fetch=>proc do |sql|
  case sql
  when /SELECT \* FROM (?:artists|albums) WHERE \(id = (\d+)\) LIMIT 1/
    {:id=>$1.to_i}
  when "SELECT id FROM albums WHERE (albums.artist_id = 1)"
    [{:id=>1}, {:id=>2}, {:id=>3}]
  when "SELECT first, last FROM vocalists WHERE (vocalists.album_id = 1)"
    [{:first=>"F1", :last=>"L1"}, {:first=>"F2", :last=>"L2"}]
  when /SELECT tag_id FROM albums_tags WHERE \(album_id = (\d)\)/
    a = []
    a << {:tag_id=>1} if $1 == '1'
    a << {:tag_id=>2} if $1 != '3'
    a << {:tag_id=>3} if $1 == '2'
    a
  when /SELECT first, last FROM albums_vocalists WHERE \(album_id = (\d)\)/
    a = []
    a << {:first=>"F1", :last=>"L1"} if $1 == '1'
    a << {:first=>"F2", :last=>"L2"} if $1 != '3'
    a << {:first=>"F3", :last=>"L3"} if $1 == '2'
    a
  when /SELECT instrument_id FROM vocalists_instruments WHERE \(\(first = '(F\d)'\) AND \(last = 'L\d'\)\)/
    a = []
    a << {:instrument_id=>1} if $1 == "F1"
    a << {:instrument_id=>2} if $1 != "F3"
    a << {:instrument_id=>3} if $1 == "F2"
    a
  when /SELECT tags\.id FROM tags INNER JOIN albums_tags/
    [{:id=>2}]
  end
end)

Artist = Class.new(Sequel::Model(DB[:artists]))
Artist.columns :id
Album = Class.new(Sequel::Model(DB[:albums]))
Album.columns :id, :artist_id
Tag = Class.new(Sequel::Model(DB[:tags]))
Tag.columns :id, :active
Vocalist = Class.new(Sequel::Model(DB[:vocalists]))
Vocalist.columns :first, :last, :album_id
Vocalist.set_primary_key [:first, :last]
Instrument = Class.new(Sequel::Model(DB[:instruments]))
Instrument.columns :id, :first, :last

Artist.plugin :association_pks
Album.plugin :association_pks
Vocalist.plugin :association_pks

Album.many_to_many :tags, :class=>Tag, :join_table=>:albums_tags, :left_key=>:album_id, :delay_pks=>false, :association_pks_differential=>true
Artist.one_to_many :albums, :class=>Album, :key=>:artist_id, :delay_pks=>false, :association_pks_differential=>true

def show(label)
  puts "== #{label} =="
  puts DB.sqls
  DB.sqls.clear
end

DB.sqls.clear

al = Album.load(:id=>2)
al.tag_pks = [1, 3]
show("m2m basic")
al.tag_pks = [2, 3]
show("m2m set to current (no-op)")
al.tag_pks = []
show("m2m empty")

ar = Artist.load(:id=>1)
ar.album_pks = [2, 4]
show("o2m basic")
ar.album_pks = [1, 2, 3]
show("o2m set to current (no-op)")

Album.many_to_many :vocalists, :class=>Vocalist, :join_table=>:albums_vocalists, :left_key=>:album_id, :right_key=>[:first, :last], :delay_pks=>false, :association_pks_differential=>true
Album.load(:id=>2).vocalist_pks = [["F1", "L1"], ["F2", "L2"]]
show("m2m right cpk")

Album.one_to_many :vocalists, :class=>Vocalist, :key=>:album_id, :delay_pks=>false, :association_pks_differential=>true
Album.load(:id=>1).vocalist_pks = [["F2", "L2"], ["F3", "L3"]]
show("o2m right cpk")

Vocalist.many_to_many :instruments, :class=>Instrument, :join_table=>:vocalists_instruments, :left_key=>[:first, :last], :delay_pks=>false, :association_pks_differential=>true
Vocalist.load(:first=>'F2', :last=>'L2').instrument_pks = [1, 2]
show("m2m left cpk")

Album.many_to_many :active_tags, :class=>Tag, :join_table=>:albums_tags, :left_key=>:album_id, :right_key=>:tag_id, :conditions=>{Sequel[:tags][:active]=>true}, :delay_pks=>false, :association_pks_differential=>true
al2 = Album.load(:id=>2)
al2.active_tag_pks = [2, 5]
show("m2m conditions add only")
al2.active_tag_pks = [5]
show("m2m conditions remove scoped")

# cache_pks short circuit
Album.many_to_many :ctags, :class=>Tag, :join_table=>:albums_tags, :left_key=>:album_id, :right_key=>:tag_id, :delay_pks=>false, :cache_pks=>true, :association_pks_differential=>true
al3 = Album.load(:id=>2)
al3.ctag_pks = [1, 3]
show("cache_pks first set")
al3.ctag_pks = [1, 3]
show("cache_pks second set (no sql)")
p al3.ctag_pks
show("cache_pks getter (no sql)")

# failure with cache_pks + transactions
DB.autoid = proc{|sql| raise Sequel::DatabaseError if sql =~ /INSERT INTO albums_tags/}
al4 = Album.load(:id=>2)
al4.use_transactions = true
begin
  al4.ctag_pks = [3, 4]
rescue Sequel::DatabaseError
  puts "raised as expected"
end
show("failure sqls")
p al4.ctag_pks
show("getter after failure (no sql, old value)")
