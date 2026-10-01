require 'minitest_helper'

describe Tomlrb::Key do
  subject { Tomlrb::Keys.new }

  it 'cannot redefine a super-table already opened by a header' do
    subject.add_table_key %w[x y z w]
    _{ subject.add_table_key %w[x] }.must_raise Tomlrb::Key::KeyConflict
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

  it 'can define a super-table already created by dotted keys' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple color], %w[fruit]
    subject.add_pair_key %w[apple taste sweet], %w[fruit]
    subject.add_table_key %w[fruit apple]
    assert true
  end

  it 'can define a parent table after a dotted key chain' do
    subject.add_table_key %w[fruit]
    subject.add_pair_key %w[apple color], %w[fruit]
    subject.add_pair_key %w[apple taste sweet], %w[fruit]
    subject.add_table_key %w[fruit apple taste]
    assert true
  end

  it 'reports both declarations when a value clashes with a table' do
    subject.add_pair_key %w[fruit apple], []
    error = assert_raises(Tomlrb::Key::KeyConflict) do
      subject.add_table_key %w[fruit apple]
    end
    _(error).must_be_kind_of Tomlrb::ParseError
    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :value
    _(error.key_path).must_equal 'apple'
  end

  it 'does not let same names in different array-of-tables instances clash' do
    subject.add_table_key %w[products], true
    subject.add_pair_key %w[name], %w[products]
    subject.add_table_key %w[products], true
    subject.add_pair_key %w[name], %w[products]
    assert true
  end
end
