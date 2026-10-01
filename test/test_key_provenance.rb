require 'minitest_helper'

describe 'key declaration provenance' do
  def conflict_for(toml)
    Tomlrb.parse(toml)
    flunk 'expected a KeyConflict'
  rescue Tomlrb::Key::KeyConflict => error
    error
  end

  it 'points at both assignments of a duplicated value' do
    error = conflict_for("a = 1\n\na = 2\n")

    _(error).must_be_kind_of Tomlrb::ParseError
    _(error.key_path).must_equal 'a'
    _(error.current.kind).must_equal :value
    _(error.current.lineno).must_equal 3
    _(error.previous.kind).must_equal :value
    _(error.previous.lineno).must_equal 1
    _(error.message).must_include 'line 3'
    _(error.message).must_include 'line 1'
  end

  it 'points at both declarations of a duplicated table' do
    error = conflict_for("[a]\n[a]\n")

    _(error.current.kind).must_equal :explicit_table
    _(error.current.lineno).must_equal 2
    _(error.previous.kind).must_equal :explicit_table
    _(error.previous.lineno).must_equal 1
  end

  it 'allows an explicit table after it was opened as an implicit parent' do
    parsed = Tomlrb.parse("a.b = 1\n[a]\nc = 2\n")

    _(parsed).must_equal('a' => { 'b' => 1, 'c' => 2 })
  end

  it 'identifies an implicit parent table when a value claims the path' do
    error = conflict_for("a.b = 1\na = 2\n")

    _(error.key_path).must_equal 'a'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :implicit_parent
    _(error.previous.lineno).must_equal 1
  end

  it 'identifies a value as the earlier declaration of a table' do
    error = conflict_for("a = 1\n[a]\n")

    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :value
  end

  it 'identifies an enclosed inline table when it is extended afterwards' do
    error = conflict_for("a = { b = 1 }\na.b = 2\n")

    _(error.key_path).must_equal 'a'
    _(error.current.kind).must_equal :implicit_parent
    _(error.current.lineno).must_equal 2
    _(error.previous.kind).must_equal :inline_table
    _(error.previous.lineno).must_equal 1
    _(error.message).must_include 'enclosed inline table'
  end

  it 'identifies an enclosed inline table when reopened with a header' do
    error = conflict_for("a = { b = 1 }\n[a]\n")

    _(error.current.kind).must_equal :explicit_table
    _(error.previous.kind).must_equal :inline_table
  end

  it 'points at both duplicated keys inside an inline table' do
    error = conflict_for("a = { b = 1, b = 2 }\n")

    _(error.key_path).must_equal 'b'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _(error.current.lineno).must_equal 1
    _(error.previous.lineno).must_equal 1
  end

  it 'points at duplicated dotted keys inside an inline table' do
    error = conflict_for("a = { b.c = 1, b.c = 2 }\n")

    _(error.key_path).must_equal 'c'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
  end

  it 'keeps bare a.b distinct from the quoted "a.b"' do
    parsed = Tomlrb.parse("a.b = 1\n\"a.b\" = 2\n")

    _(parsed).must_equal('a' => { 'b' => 1 }, 'a.b' => 2)
  end

  it 'still clashes when the same quoted key is repeated' do
    error = conflict_for("\"a.b\" = 1\n\"a.b\" = 2\n")

    _(error.key_path).must_equal '"a.b"'
    _(error.current.kind).must_equal :value
    _(error.previous.kind).must_equal :value
    _(error.previous.lineno).must_equal 1
    _(error.current.lineno).must_equal 2
  end

  it 'does not clash across distinct array-of-tables instances' do
    parsed = Tomlrb.parse("[[products]]\nname = 1\n\n[[products]]\nname = 2\n")

    _(parsed).must_equal('products' => [{ 'name' => 1 }, { 'name' => 2 }])
  end

  it 'clashes on duplicate keys within a single array-of-tables instance' do
    error = conflict_for("[[products]]\nname = 1\nname = 2\n")

    _(error.key_path).must_equal 'name'
    _(error.previous.lineno).must_equal 2
    _(error.current.lineno).must_equal 3
  end

  it 'points at the array table when a normal table reuses its name' do
    error = conflict_for("[[a]]\n[a]\n")

    _(error.previous.kind).must_equal :array_table
    _(error.current.kind).must_equal :explicit_table
  end

  it 'allows back-declaring parent tables between conflicting assignments' do
    parsed = Tomlrb.parse(<<~TOML)
      a.b.c = 1
      [a]
      d = 2
      [a.b]
      e = 3
    TOML

    _(parsed).must_equal(
      'a' => {
        'b' => { 'c' => 1, 'e' => 3 },
        'd' => 2
      }
    )
  end

  it 'reports the later overwrite when interleaving parent back-declarations' do
    error = conflict_for(<<~TOML)
      a.b.c = 1
      [a]
      d = 2
      [a.b]
      c = 3
    TOML

    _(error.key_path).must_equal 'c'
    _(error.previous.lineno).must_equal 1
    _(error.current.lineno).must_equal 5
  end
end
