# frozen_string_literal: true
require 'support'
require 'mustermann/match'
require 'mustermann/sinatra'

describe Mustermann::Match do
  let(:pattern) { Mustermann::Sinatra.new('/:name') }
  subject(:match) { pattern.match('/foo') }

  # For a full match, string and to_s are both the matched string.
  its(:string)     { should be == '/foo'            }
  its(:to_s)       { should be == '/foo'            }
  its(:params)     { should be == { 'name' => 'foo' } }
  its(:post_match) { should be == ''                }
  its(:pre_match)  { should be == ''                }
  its(:to_h)       { should be == { 'name' => 'foo' } }
  its(:captures)        { should be == ['foo'] }
  its(:named_captures)  { should be == { 'name' => 'foo' } }
  its(:names)           { should be == ['name'] }

  # For a peek match, #string is the full searched string and #to_s is the matched portion.
  describe 'peek match' do
    subject(:peek) { pattern.peek_match('/foo/bar') }
    its(:string) { should be == '/foo/bar' }
    its(:to_s)   { should be == '/foo'     }
  end

  describe :[] do
    example('symbol key')  { match[:name].should  be == 'foo' }
    example('string key')  { match['name'].should be == 'foo' }
    example('integer key') { match[0].should      be == 'foo' }
    example('range key')   { match[0..0].should   be == ['foo'] }
    example('invalid key') do
      expect { match[1.0] }.to raise_error(ArgumentError, /key must be/)
    end
  end

  describe :values_at do
    example { match.values_at(:name, 'name').should be == ['foo', 'foo'] }
  end

  describe :deconstruct_keys do
    example { match.deconstruct_keys([:name]).should be == { name: 'foo' } }
  end

  describe :spans do
    let(:path) { '/%E4%B8%AD%E6%96%87/x%2Fy/x%2Fy' }
    let(:pattern_with_dup) { Mustermann::Sinatra.new('/:city/:id/:id') }
    subject(:m) { pattern_with_dup.match(path) }

    example('keeps the undecoded text') do
      m.span(:city).raw.should be == '%E4%B8%AD%E6%96%87'
      m.params['city'].should be == '中文'
    end

    example('does not treat %2F as a separator') do
      m.span('id').first.raw.should be == 'x%2Fy'
    end

    example('reports half-open byte offsets relative to the original string') do
      city = m.span(:city)
      city.begin_byte.should be == 1
      city.end_byte.should   be == 19
      city.byte_range.should be == (1...19)
      city.bytesize.should   be == 18
      path.byteslice(city.begin_byte, city.bytesize).should be == '%E4%B8%AD%E6%96%87'
    end

    example('slices identical original bytes even for two equal captures') do
      ids = m.span(:id)
      ids.should be_a(Array)
      ids.map(&:raw).should be == ['x%2Fy', 'x%2Fy']
      ids.map { |span| path.byteslice(span.begin_byte, span.bytesize) }.should be == ['x%2Fy', 'x%2Fy']
      ids.map(&:begin_byte).should be == [20, 26]
      m.span(1).should be == ids.first
      m.span(2).should be == ids.last
    end

    example('exposes one positional span per capture group') do
      m.spans.map { |span| span && span.raw }.should be ==
        ['%E4%B8%AD%E6%96%87', 'x%2Fy', 'x%2Fy']
    end

    example('supports spans_at') do
      m.spans_at(:city, 'id').first.raw.should be == '%E4%B8%AD%E6%96%87'
      m.spans_at('id').last.map(&:raw).should be == ['x%2Fy', 'x%2Fy']
    end

    describe 'missing optional segment vs empty capture' do
      let(:optional) { Mustermann::Sinatra.new('/:a(.:ext)?') }

      example('absent optional segment is nil, not a zero-length span') do
        result = optional.match('/foo')
        result.span(:a).empty?.should be false
        result.span(:ext).should be_nil
        result.spans.map { |span| span && span.raw }.should be == ['foo', nil]
        result.named_spans.should be == { 'a' => result.span(:a) }
      end

      example('a present capture has a span, even when decoded to the same value') do
        result = optional.match('/foo.png')
        result.span(:ext).raw.should be == 'png'
      end

      example('a capture matching the empty string is a zero-length span') do
        pattern = Mustermann::Sinatra.new('/:a/*rest')
        result  = pattern.match('/foo/')
        rest    = result.span(:rest)
        rest.should_not be_nil
        rest.empty?.should be true
        rest.begin_byte.should be == rest.end_byte
        rest.raw.should be == ''
      end
    end

    describe 'raw multibyte input' do
      let(:path) { '/中文/a/a' }

      example('uses byte offsets for multibyte characters') do
        result = pattern_with_dup.match(path)
        city   = result.span(:city)
        city.raw.should be == '中文'
        city.begin_byte.should be == 1
        city.end_byte.should   be == 7
        path.byteslice(city.begin_byte, city.bytesize).should be == '中文'
        result.span(:id).map(&:raw).should be == ['a', 'a']
      end
    end

    describe 'peek match' do
      let(:pattern) { Mustermann::Sinatra.new('/:city') }

      example('offsets are relative to the full string, not the matched prefix') do
        full   = '/%E4%B8%AD/tail'
        result = pattern.peek_match(full)
        result.span(:city).begin_byte.should be == 1
        result.span(:city).end_byte.should   be == 10
        full.byteslice(1, 9).should be == '%E4%B8%AD'
      end
    end

    describe Mustermann::Span do
      subject(:span) { m.span(:city) }

      example('supports == and eql?') do
        span.should be == Mustermann::Span.new(path, 1, 19)
        span.should be_eql(Mustermann::Span.new(path, 1, 19))
        span.should_not be == Mustermann::Span.new(path, 1, 18)
      end

      example('is comparable') do
        first, second = m.span(:id)
        first.should be < second
      end

      example('inspects offsets and raw value') do
        span.inspect.should be == '#<Mustermann::Span: 1...19 "%E4%B8%AD%E6%96%87">'
      end
    end
  end

  describe :eql? do
    example { match.eql?(pattern.match('/foo')).should be true  }
    example { match.eql?(pattern.match('/bar')).should be false }
    example { match.eql?('not a match').should         be false }
  end

  describe :hash do
    example { match.hash.should be == pattern.match('/foo').hash }
    example { match.hash.should_not be == pattern.match('/bar').hash }
  end

  describe :== do
    example { (match == pattern.match('/foo')).should be true  }
    example { (match == pattern.match('/bar')).should be false }
  end

  describe :inspect do
    example('with a capture') { match.inspect.should be == '#<Mustermann::Match: "/foo" name:"foo">' }

    example('without captures') do
      Mustermann::Sinatra.new('/hello').match('/hello').inspect.should be == '#<Mustermann::Match: "/hello">'
    end

    example('multiple captures') do
      Mustermann::Sinatra.new('/:a/:b').match('/x/y').inspect.should be == '#<Mustermann::Match: "/x/y" a:"x" b:"y">'
    end

    example('type-converted capture shows converted value') do
      Mustermann::Sinatra.new('/:n', capture: Integer).match('/42').inspect.should be == '#<Mustermann::Match: "/42" n:42>'
    end
  end

  describe :pretty_print do
    example('with a capture') { PP.pp(match, +'').chomp.should be == '#<Mustermann::Match: "/foo" name:"foo">' }

    example('without captures') do
      PP.pp(Mustermann::Sinatra.new('/hello').match('/hello'), +'').chomp.should be == '#<Mustermann::Match: "/hello">'
    end
  end

  describe 'invalid constructor arguments' do
    example('bad first argument') do
      expect { Mustermann::Match.new('not a pattern', '/foo') }.to raise_error(ArgumentError, /first argument/)
    end

    example('bad second argument') do
      expect { Mustermann::Match.new(pattern, 42) }.to raise_error(ArgumentError, /second argument/)
    end
  end
end
