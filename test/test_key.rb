require 'minitest_helper'

describe Tomlrb::Key do
  subject { Tomlrb::Keys.new }

  def part(name, line = nil, column = nil)
    location = line.nil? ? nil : Tomlrb::Location.new(line, column)
    Tomlrb::KeyPart.new(name, location)
  end

  it 'can define a super-table afterward' do
    subject.add_table_key %w[x y z w]
    subject.add_table_key %w[x]
    assert true
  end

  it 'cannot define a table more than once' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple], %w[fruit]
    _{ subject.add_table_key %w[fruit] }.must_raise Tomlrb::Key::KeyConflict
  end

  it 'cannot define a table more than once' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple], %w[fruit]
    _{ subject.add_table_key %w[fruit apple] }.must_raise Tomlrb::Key::KeyConflict
  end

  it 'can define tables out of order' do
    subject.add_table_key %w[fruit apple]
    subject.add_table_key %w[animal]
    subject.add_table_key %w[fruit orange]
    assert true
  end

  it 'is recommended to define tables in order' do
    subject.add_table_key %w[fruit apple]
    subject.add_table_key %w[fruit orange]
    subject.add_table_key %w[animal]
    assert true
  end

  it 'can define sub-tables within tables defined via dotted keys' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple color], %w[fruit]
    subject.add_pair_key %w[apple taste sweet], %w[fruit]
    subject.add_table_key %w[fruit apple texture]
    subject.add_pair_key %w[smooth], %w[fruit apple texture]
    assert true
  end

  it 'cannot define table key already defined as pair key' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple color], %w[fruit]
    subject.add_pair_key %w[apple taste sweet], %w[fruit]
   _{ subject.add_table_key %w[fruit apple] }.must_raise Tomlrb::Key::KeyConflict
  end

  it 'cannot define table key already defined as pair key' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple color], %w[fruit]
    subject.add_pair_key %w[apple taste sweet], %w[fruit]
   _{ subject.add_table_key %w[fruit apple taste] }.must_raise Tomlrb::Key::KeyConflict
  end

  it 'attaches the two sources of a duplicate pair declaration' do
    subject.add_pair_key [part('a', 1, 1)], []
    error = begin
      subject.add_pair_key [part('a', 4, 1)], []
      flunk 'expected KeyConflict'
    rescue Tomlrb::Key::KeyConflict => conflict
      conflict
    end
    _(error.key_name).must_equal 'a'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _(error.current.location.line).must_equal 4
    _(error.previous.location.line).must_equal 1
  end

  it 'reports an implicit parent table conflicting with a value' do
    subject.add_pair_key [part('a', 1, 1), part('b', 1, 3)], []
    error = begin
      subject.add_pair_key [part('a', 2, 1), part('b', 2, 3), part('c', 2, 5)], []
      flunk 'expected KeyConflict'
    rescue Tomlrb::Key::KeyConflict => conflict
      conflict
    end
    _(error.key_name).must_equal 'b'
    _(error.current.kind).must_equal :implicit_table
    _(error.previous.kind).must_equal :value
  end

  it 'isolates array-of-tables instances but flags duplicates inside one' do
    subject.add_table_key [part('fruit', 1, 3)], true
    subject.add_pair_key [part('name', 2, 1)], [part('fruit', 1, 3)]
    subject.add_table_key [part('fruit', 3, 3)], true
    subject.add_pair_key [part('name', 4, 1)], [part('fruit', 3, 3)]

    error = begin
      subject.add_pair_key [part('name', 5, 1)], [part('fruit', 3, 3)]
      flunk 'expected KeyConflict'
    rescue Tomlrb::Key::KeyConflict => conflict
      conflict
    end
    _(error.key_name).must_equal 'name'
    _(error.current.location.line).must_equal 5
    _(error.previous.location.line).must_equal 4
  end
end
