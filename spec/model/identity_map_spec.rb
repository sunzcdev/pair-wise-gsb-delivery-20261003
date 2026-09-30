# frozen_string_literal: true
require_relative "spec_helper"

describe Sequel::Model, "#with_identity_map" do
  before do
    class ::IMArtist < Sequel::Model(:artists)
      columns :id, :name
      one_to_many :albums, :class=>'IMAlbum', :key=>:artist_id
    end

    class ::IMAlbum < Sequel::Model(:albums)
      columns :id, :artist_id
      many_to_one :artist, :class=>'IMArtist', :key=>:artist_id
      one_to_many :tracks, :class=>'IMTrack', :key=>:album_id
    end

    class ::IMTrack < Sequel::Model(:tracks)
      columns :id, :album_id, :artist_id
      many_to_one :artist, :class=>'IMArtist', :key=>:artist_id
    end

    DB.reset
  end
  after do
    [:IMArtist, :IMAlbum, :IMTrack].each{|x| Object.send(:remove_const, x)}
  end

  it "should use the same object when the same record is eagerly loaded via multiple association paths" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2}])
    IMTrack.dataset = IMTrack.dataset.with_fetch([{:id=>3, :album_id=>1, :artist_id=>2}])
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    album = IMAlbum.with_identity_map.eager(:artist, :tracks=>:artist).all.first
    album.artist.object_id.must_equal album.tracks.first.artist.object_id
  end

  it "should use different objects for the same record when not enabled" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2}])
    IMTrack.dataset = IMTrack.dataset.with_fetch([{:id=>3, :album_id=>1, :artist_id=>2}])
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    album = IMAlbum.eager(:artist, :tracks=>:artist).all.first
    album.artist.object_id.wont_equal album.tracks.first.artist.object_id
  end

  it "should not merge records when the primary key is not part of the projection" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:artist_id=>2}, {:artist_id=>2}])
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    albums = IMAlbum.select(:artist_id).with_identity_map.eager(:artist).all
    albums.length.must_equal 2
    albums[0].object_id.wont_equal albums[1].object_id
    albums[0].artist.object_id.must_equal albums[1].artist.object_id
  end

  it "should not erase previously loaded column values when a later load has fewer columns" do
    IMArtist.dataset = IMArtist.dataset.with_fetch(proc do |sql|
      if sql =~ /WHERE/
        [{:id=>2}]
      else
        [{:id=>2, :name=>'A'}]
      end
    end)
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2}])
    artist = IMArtist.with_identity_map.eager(:albums=>:artist).all.first
    artist.name.must_equal 'A'
    artist.albums.first.artist.object_id.must_equal artist.object_id
    artist.albums.first.artist.name.must_equal 'A'
  end

  it "should add column values from later loads that were not previously loaded" do
    IMArtist.dataset = IMArtist.dataset.with_fetch(proc do |sql|
      if sql =~ /WHERE/
        [{:id=>2, :name=>'A'}]
      else
        [{:id=>2}]
      end
    end)
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2}])
    artist = IMArtist.with_identity_map.eager(:albums=>:artist).all.first
    artist.albums.first.artist.object_id.must_equal artist.object_id
    artist.name.must_equal 'A'
  end

  it "should use the same object for eager_graph when the same record appears via multiple table aliases" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2, :artist_id_0=>2, :artist_name=>'A', :tracks_id=>3, :tracks_album_id=>1, :tracks_artist_id=>2, :artist_0_id=>2, :artist_0_name=>'A'}])
    album = IMAlbum.with_identity_map.eager_graph(:artist, :tracks=>:artist).all.first
    album.artist.object_id.must_equal album.tracks.first.artist.object_id
  end

  it "should use different objects for eager_graph when not enabled" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2, :artist_id_0=>2, :artist_name=>'A', :tracks_id=>3, :tracks_album_id=>1, :tracks_artist_id=>2, :artist_0_id=>2, :artist_0_name=>'A'}])
    album = IMAlbum.eager_graph(:artist, :tracks=>:artist).all.first
    album.artist.object_id.wont_equal album.tracks.first.artist.object_id
  end

  it "should share objects between eager_graph and eager used in the same query" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2, :artist_id_0=>2, :artist_name=>'A'}])
    IMTrack.dataset = IMTrack.dataset.with_fetch([{:id=>3, :album_id=>1, :artist_id=>2}])
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    album = IMAlbum.with_identity_map.eager_graph(:artist).eager(:tracks=>:artist).all.first
    album.artist.object_id.must_equal album.tracks.first.artist.object_id
  end

  it "should handle associations without any matching records" do
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([])
    artist = IMArtist.with_identity_map.eager(:albums).all.first
    artist.albums.must_equal []
  end

  it "should handle many_to_one associations with nil key values" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>nil}])
    album = IMAlbum.with_identity_map.eager(:artist).all.first
    album.artist.must_be_nil
  end

  it "should handle eager_graph associations without any matching records" do
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A', :albums_id=>nil, :albums_artist_id=>nil}])
    artist = IMArtist.with_identity_map.eager_graph(:albums).all.first
    artist.albums.must_equal []
  end

  it "should not share objects across separate queries" do
    IMAlbum.dataset = IMAlbum.dataset.with_fetch([{:id=>1, :artist_id=>2}])
    IMArtist.dataset = IMArtist.dataset.with_fetch([{:id=>2, :name=>'A'}])
    ds = IMAlbum.with_identity_map.eager(:artist)
    a1 = ds.all.first
    a2 = ds.all.first
    a1.object_id.wont_equal a2.object_id
    a1.artist.object_id.wont_equal a2.artist.object_id
  end
end

describe Sequel::Model, "#with_identity_map with composite primary keys" do
  before do
    class ::IMVersion < Sequel::Model(:versions)
      columns :id1, :id2, :name
      set_primary_key [:id1, :id2]
      one_to_many :items, :class=>'IMItem', :key=>[:v1, :v2], :primary_key=>[:id1, :id2]
    end

    class ::IMItem < Sequel::Model(:items)
      columns :id, :v1, :v2
      many_to_one :version, :class=>'IMVersion', :key=>[:v1, :v2], :primary_key=>[:id1, :id2]
    end

    DB.reset
  end
  after do
    [:IMVersion, :IMItem].each{|x| Object.send(:remove_const, x)}
  end

  it "should use the full composite primary key as part of the identity" do
    IMVersion.dataset = IMVersion.dataset.with_fetch([{:id1=>1, :id2=>2, :name=>'v'}, {:id1=>1, :id2=>3, :name=>'w'}])
    IMItem.dataset = IMItem.dataset.with_fetch([{:id=>5, :v1=>1, :v2=>2}, {:id=>6, :v1=>1, :v2=>3}])
    versions = IMVersion.with_identity_map.eager(:items=>:version).all
    versions.length.must_equal 2
    versions[0].items.length.must_equal 1
    versions[1].items.length.must_equal 1
    versions[0].items[0].version.object_id.must_equal versions[0].object_id
    versions[1].items[0].version.object_id.must_equal versions[1].object_id
    versions[0].items[0].version.object_id.wont_equal versions[1].items[0].version.object_id
  end

  it "should not merge records missing part of the composite primary key" do
    IMVersion.dataset = IMVersion.dataset.with_fetch([{:id1=>1, :name=>'v'}, {:id1=>1, :name=>'w'}])
    versions = IMVersion.select(:id1, :name).with_identity_map.all
    versions.length.must_equal 2
    versions[0].object_id.wont_equal versions[1].object_id
    versions.map{|v| v.name}.must_equal ['v', 'w']
  end
end
