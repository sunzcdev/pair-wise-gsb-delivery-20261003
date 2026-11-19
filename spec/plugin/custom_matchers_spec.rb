require_relative "../spec_helper"
require 'set'

describe "custom_matchers plugin" do 
  it "supports matching for custom classes and objects" do
    app(:bare) do
      plugin :custom_matchers

      custom_matcher(Set){|set| set.any?{|i| match(i)}}

      c = Struct.new(:meth, :start)
      custom_matcher(c) do |s|
        if request_method == s.meth && remaining_path.start_with?(s.start)
          captures << self.GET['foo']
        end
      end

      o = Object.new
      o2 = Object.new
      o.define_singleton_method(:===){|other| other == o2}
      custom_matcher(o){|s| match(/o3-(\d+)/)}

      route do |r|
        r.is(Set.new(['a', 'b'])){'c'}
        r.on(c.new('GET', '/d')){|x| "e#{x.inspect}"}
        r.is(o2){|x| "f#{x}"}
        r.is("x", Object.new){'v'}
        'g'
      end
    end

    body.must_equal 'g'
    body('/a').must_equal 'c'
    body('/b').must_equal 'c'
    body('/a/').must_equal 'g'
    body('/d').must_equal 'enil'
    body('/d', 'QUERY_STRING'=>'foo=bar').must_equal 'e"bar"'
    body('/d', 'REQUEST_METHOD'=>'POST').must_equal 'g'
    body('/o3').must_equal 'g'
    body('/o3-').must_equal 'g'
    body('/o3-123').must_equal 'f123'
    body('/o3-123/').must_equal 'g'
    body('/o3-a').must_equal 'g'
    proc{body('/x')}.must_raise Roda::RodaError
  end

  it "works when overriding methods in subclasses" do
    c = Struct.new(:meth, :start)
    app(:bare) do
      plugin :custom_matchers

      custom_matcher(c) do |s|
        if request_method == s.meth && remaining_path.start_with?(s.start)
          captures << self.GET['foo']
        end
      end
      route do |r|
        r.on(c.new('GET', '/d')){|x| "e#{x.inspect}"}
      end
    end

    body('/d', 'QUERY_STRING'=>'foo=bar').must_equal 'e"bar"'
    body('/d', 'QUERY_STRING'=>'bar=foo').must_equal 'enil'

    @app = Class.new(@app)
    app.custom_matcher(c) do |s|
      if request_method == s.meth && remaining_path.start_with?(s.start)
        captures << self.GET['bar']
      end
    end

    @app.freeze
    proc{app.custom_matcher(c){|s|}}.must_raise(RuntimeError)

    body('/d', 'QUERY_STRING'=>'foo=bar').must_equal 'enil'
    body('/d', 'QUERY_STRING'=>'bar=foo').must_equal 'e"foo"'
  end
end
