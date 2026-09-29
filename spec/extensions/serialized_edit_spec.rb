# frozen_string_literal: true
require_relative "spec_helper"

require 'yaml'

describe "serialized_edit plugin" do
  before do
    @c = Class.new(Sequel::Model(:items))
    @c.class_eval do
      columns :id, :h
      plugin :serialized_edit
      serialize_attributes :yaml, :h
    end
    DB.reset
  end

  def nested_value
    {'tags'=>['a', 'b'], 'meta'=>{'level'=>1, 'flags'=>['x']}}
  end

  def load_item(value=nested_value)
    @c.load(:id=>1, :h=>YAML.dump(value))
  end

  it "should commit nested modifications made by the block" do
    o = load_item
    original = o.h
    ret = o.edit_serialized(:h) do |h|
      h['tags'] << 'c'
      h['meta']['level'] = 2
      h['meta']['flags'].delete('x')
    end
    expected = {'tags'=>['a', 'b', 'c'], 'meta'=>{'level'=>2, 'flags'=>[]}}
    ret.must_equal expected
    ret.must_be_same_as o.h
    o.h.must_equal expected
    o.changed_columns.must_equal [:h]
    original.must_equal nested_value
  end

  it "should roll back nested modifications when validation rejects the column" do
    @c.class_eval do
      def validate
        super
        errors.add(:h, 'cannot grant admin') if h && h['role'] == 'admin'
      end
    end
    o = load_item
    original = o.h
    ret = o.edit_serialized(:h) do |h|
      h['meta']['level'] = 99
      h['role'] = 'admin'
    end
    ret.must_be_nil
    o.h.must_be_same_as original
    o.h.must_equal nested_value
    o.changed_columns.must_equal []
    o.errors.on(:h).must_equal ['cannot grant admin']
  end

  it "should not modify the model when the block raises an exception" do
    o = load_item
    original = o.h
    proc do
      o.edit_serialized(:h) do |h|
        h['meta']['level'] = 99
        raise ArgumentError
      end
    end.must_raise(ArgumentError)
    o.h.must_be_same_as original
    o.h.must_equal nested_value
    o.changed_columns.must_equal []
  end

  it "should isolate the yielded value from the model during and after the block" do
    o = load_item
    yielded = nil
    o.edit_serialized(:h) do |h|
      yielded = h
      h['tags'] << 'c'
      o.h.must_equal nested_value
    end
    o.h.must_equal({'tags'=>['a', 'b', 'c'], 'meta'=>{'level'=>1, 'flags'=>['x']}})

    yielded['tags'] << 'd'
    yielded['meta']['level'] = 99
    o.h.must_equal({'tags'=>['a', 'b', 'c'], 'meta'=>{'level'=>1, 'flags'=>['x']}})
  end

  it "should preserve shared subobjects inside the yielded copy while isolating them from the model" do
    shared = {'x'=>1}
    o = @c.new
    o.h = {'a'=>shared, 'b'=>shared}
    o.edit_serialized(:h) do |h|
      h['a'].must_be_same_as h['b']
      h['a']['x'] = 2
    end
    shared.must_equal({'x'=>1})
    o.h['a'].must_equal({'x'=>2})
    o.h['a'].must_be_same_as o.h['b']
  end

  it "should support editing frozen values" do
    value = nested_value
    value['tags'].freeze
    value['meta']['flags'].freeze
    value['meta'].freeze
    value.freeze
    o = @c.new
    o.h = value
    o.edit_serialized(:h) do |h|
      h.frozen?.must_equal false
      h['tags'] << 'c'
      h['meta']['level'] = 2
    end
    o.h.must_equal({'tags'=>['a', 'b', 'c'], 'meta'=>{'level'=>2, 'flags'=>['x']}})
    o.h.frozen?.must_equal false
    value.must_equal nested_value
    value.must_be :frozen?
  end

  it "should be a no-op if the block does not modify the value" do
    o = load_item
    current = o.h
    ret = o.edit_serialized(:h){|h| h['tags'].length}
    ret.must_be_same_as current
    o.changed_columns.must_equal []
  end

  it "should only consider validation errors on the column being edited" do
    @c.class_eval do
      def validate
        super
        errors.add(:id, 'is invalid')
      end
    end
    o = load_item
    o.edit_serialized(:h){|h| h['tags'] << 'c'}.wont_be_nil
    o.h['tags'].must_equal ['a', 'b', 'c']
    o.errors.must_be_empty
  end

  it "should serialize the committed value when saving" do
    o = load_item
    o.edit_serialized(:h){|h| h['tags'] << 'c'}
    o.save_changes
    expected = {'tags'=>['a', 'b', 'c'], 'meta'=>{'level'=>1, 'flags'=>['x']}}
    DB.sqls.must_equal ["UPDATE items SET h = '#{YAML.dump(expected)}' WHERE (id = 1)"]
  end

  it "should work with the dirty plugin when committing" do
    @c.plugin :dirty
    o = load_item
    o.edit_serialized(:h){|h| h['meta']['level'] = 2}
    o.column_changed?(:h).must_equal true
    o.column_changes.must_equal(:h=>[nested_value, {'tags'=>['a', 'b'], 'meta'=>{'level'=>2, 'flags'=>['x']}}])
  end

  it "should work with the dirty plugin when rolling back" do
    @c.plugin :dirty
    @c.class_eval do
      def validate
        super
        errors.add(:h, 'level too high') if h && h['meta']['level'] > 10
      end
    end
    o = load_item
    o.edit_serialized(:h){|h| h['meta']['level'] = 99}.must_be_nil
    o.changed_columns.must_equal []
    o.column_changes.must_equal({})
  end

  it "should work with serialization_modification_detection when committing" do
    @c.plugin :serialization_modification_detection
    o = load_item
    o.changed_columns.must_equal []
    o.edit_serialized(:h){|h| h['tags'] << 'c'}
    o.changed_columns.must_equal [:h]
    o.save_changes
    o.changed_columns.must_equal []
  end

  it "should work with serialization_modification_detection when rolling back" do
    @c.plugin :serialization_modification_detection
    @c.class_eval do
      def validate
        super
        errors.add(:h, 'level too high') if h && h['meta']['level'] > 10
      end
    end
    o = load_item
    o.edit_serialized(:h){|h| h['meta']['level'] = 99}.must_be_nil
    o.changed_columns.must_equal []
  end

  it "should work with refresh" do
    o = load_item
    o.edit_serialized(:h){|h| h['tags'] << 'c'}
    o.save_changes

    @c.dataset = @c.dataset.with_fetch(:id=>1, :h=>YAML.dump({'tags'=>['z'], 'meta'=>{'level'=>5, 'flags'=>[]}}))
    o.refresh
    o.h.must_equal({'tags'=>['z'], 'meta'=>{'level'=>5, 'flags'=>[]}})
    o.changed_columns.must_equal []

    o.edit_serialized(:h){|h| h['meta']['level'] = 6}
    o.h.must_equal({'tags'=>['z'], 'meta'=>{'level'=>6, 'flags'=>[]}})
    o.changed_columns.must_equal [:h]
  end

  it "should handle nil values" do
    o = @c.load(:id=>1, :h=>nil)
    o.edit_serialized(:h){|h| h}.must_be_nil
    o.changed_columns.must_equal []
  end

  it "should raise an error for non-serialized columns" do
    o = load_item
    proc{o.edit_serialized(:id){|v| v}}.must_raise(Sequel::Error)
  end

  it "should raise an error when not given a block" do
    o = load_item
    proc{o.edit_serialized(:h)}.must_raise(ArgumentError)
  end
end
