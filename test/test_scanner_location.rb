require 'minitest_helper'
require 'stringio'

describe Tomlrb::Scanner do
  def tokens(source)
    scanner = Tomlrb::Scanner.new(StringIO.new(source))
    result = []
    while (token = scanner.next_token)
      result << [token[0], token[1], scanner.location.dup]
    end
    result
  end

  it 'tracks line and column of identifiers' do
    location = tokens("a = 1").first.last
    _(location.line).must_equal 1
    _(location.column).must_equal 1

    identifier = tokens("\n  abc = 1").find { |symbol, _, _| symbol == :IDENTIFIER }
    _(identifier[2].line).must_equal 2
    _(identifier[2].column).must_equal 3
  end

  it 'advances lines over multiline basic strings' do
    scanned = tokens("a = \"\"\"\nline\n\"\"\"\nb = 2")
    b_token = scanned.find { |symbol, value, _location| symbol == :IDENTIFIER && value == 'b' }
    _(b_token[2].line).must_equal 4
    _(b_token[2].column).must_equal 1
  end

  it 'advances lines over CRLF newlines' do
    scanned = tokens("a = 1\r\nb = 2")
    b_token = scanned.find { |symbol, value, _location| symbol == :IDENTIFIER && value == 'b' }
    _(b_token[2].line).must_equal 2
    _(b_token[2].column).must_equal 1
  end

  it 'counts columns of every segment in a dotted float key' do
    parser_tokens = tokens("1.2 = 1")
    float = parser_tokens.find { |symbol, _value, _location| symbol == :FLOAT }
    _(float[2].column).must_equal 1
  end
end
