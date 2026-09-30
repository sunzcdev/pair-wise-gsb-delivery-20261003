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

  describe 'capture spans' do
    let(:pattern) { Mustermann::Sinatra.new('/:x/:y/:x') }
    let(:path)    { '/%E4%B8%AD%E6%96%87/a%2Fb/%E4%B8%AD%E6%96%87' }
    subject(:m)   { pattern.match(path) }

    it 'exposes one occurrence per group, in left-to-right order' do
      expect(m.capture_occurrences.map(&:name)).to eq %w[x y x]
    end

    it 'reports half-open byte spans relative to the original string' do
      expect(m.capture_occurrences.map(&:span)).to eq [[1, 19], [20, 25], [26, 44]]
      expect(m.capture_occurrences.map(&:range)).to eq [1...19, 20...25, 26...44]
    end

    it 'slices the exact raw bytes back out of the original string' do
      raw = m.capture_occurrences.map { |c| path.byteslice(c.start, c.length) }
      expect(raw).to eq ['%E4%B8%AD%E6%96%87', 'a%2Fb', '%E4%B8%AD%E6%96%87']
    end

    it 'offers raw text on the occurrence itself' do
      expect(m.capture_occurrences.map(&:bytes)).to eq ['%E4%B8%AD%E6%96%87', 'a%2Fb', '%E4%B8%AD%E6%96%87']
    end

    it 'keeps raw text undecoded while params stay decoded' do
      expect(m.params).to eq('x' => ['中文', '中文'], 'y' => 'a/b')
      expect(m.raw_params).to eq('x' => ['%E4%B8%AD%E6%96%87', '%E4%B8%AD%E6%96%87'], 'y' => 'a%2Fb')
    end

    it 'distinguishes the two occurrences even with identical content' do
      match = pattern.match('/a%2Fb/c/a%2Fb')
      expect(match.capture_occurrences.map(&:span)).to eq [[1, 6], [7, 8], [9, 14]]
      expect(match.capture('x').map(&:bytes)).to eq ['a%2Fb', 'a%2Fb']
      expect(match.capture('y').bytes).to eq 'c'
    end

    it 'looks up occurrences by integer index' do
      expect(m.capture(0).name).to eq 'x'
      expect(m.capture(2).bytes).to eq '%E4%B8%AD%E6%96%87'
    end

    it 'marks an absent optional capture as not present' do
      match = Mustermann::Sinatra.new('/:a(/:b)?/:z').match('/foo/bar')
      absent = match.capture_occurrences.find { |c| c.name == 'b' }
      expect(absent.present?).to be false
      expect(absent.span).to eq [nil, nil]
      expect(absent.bytes).to be_nil
      expect(absent.range).to be_nil
      present = match.capture_occurrences.find { |c| c.name == 'z' }
      expect(present.present?).to be true
    end

    it 'marks an empty-but-present splat capture as present and empty' do
      match = Mustermann::Sinatra.new('/:a/*splat').match('/foo/')
      splat = match.capture_occurrences.last
      expect(splat.name).to eq 'splat'
      expect(splat.present?).to be true
      expect(splat.empty?).to be true
      expect(splat.bytes).to eq ''
      expect(splat.span).to eq [5, 5]
      expect(match.raw_params['splat']).to eq ['']
    end

    it 'reports offsets relative to the full string when peeking' do
      peek = pattern.peek_match(path + '/extra')
      expect(peek.to_s).to eq path
      expect(peek.post_match).to eq '/extra'
      expect(peek.capture_occurrences.map(&:span)).to eq [[1, 19], [20, 25], [26, 44]]
      raw = peek.capture_occurrences.map { |c| peek.string.byteslice(c.start, c.length) }
      expect(raw).to eq ['%E4%B8%AD%E6%96%87', 'a%2Fb', '%E4%B8%AD%E6%96%87']
    end

    it 'exposes raw captures in left-to-right order' do
      expect(m.raw_captures).to eq ['%E4%B8%AD%E6%96%87', 'a%2Fb', '%E4%B8%AD%E6%96%87']
    end
  end
end
