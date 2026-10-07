# frozen_string_literal: true
require 'support'
require 'mustermann'

describe Mustermann::Incremental do
  def session_for(pattern_string, **options)
    Mustermann::Incremental.new(Mustermann.new(pattern_string, **options))
  end

  def feed(session, input, chunking)
    case chunking
    when :bytes then input.bytes.each { |byte| session << byte.chr(Encoding::BINARY) }
    when :chars then input.chars.each { |char| session << char }
    when Integer then input.scan(/.{1,#{chunking}}/m).each { |chunk| session << chunk }
    else session << input
    end
    session
  end

  describe 'session creation' do
    it 'rejects patterns with look-aheads' do
      expect { session_for('/:name(.:ext)?') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'rejects patterns with unions' do
      expect { session_for('/(a|b)') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'rejects patterns with the :capture option' do
      expect { session_for('/:name', capture: :word) }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'rejects patterns with the :except option' do
      expect { session_for('/:name', except: '/admin') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'rejects non-AST patterns' do
      expect { Mustermann::Incremental.new(Mustermann.new(/.*/, type: :regexp)) }.
        to raise_error(Mustermann::Incremental::UnsupportedPatternError)
      expect { Mustermann::Incremental.new(Mustermann.new('/foo', type: :identity)) }.
        to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'accepts literals, captures, optional segments and splats' do
      expect { session_for('/foo/:name(/:id)?/*rest') }.not_to raise_error
    end
  end

  describe 'status reporting' do
    subject(:session) { session_for('/foo/:name') }

    it 'starts out as :continue' do
      session.status.should be == :continue
    end

    it 'returns :continue while the input can still lead to a match' do
      (session << '/fo').should be == :continue
      (session << 'o/b').should be == :continue
      session.status.should be == :continue
    end

    it 'returns :mismatch once no matching input is possible anymore' do
      session << '/fo'
      (session << 'x').should be == :mismatch
      session.status.should be == :mismatch
    end

    it 'stays :mismatch when more input is fed' do
      session << '/fox'
      (session << '/foo').should be == :mismatch
    end

    it 'reports :match after a successful finish' do
      session << '/foo/bar'
      session.finish
      session.status.should be == :match
    end

    it 'reports :mismatch after an unsuccessful finish' do
      session << '/foo'
      session.finish
      session.status.should be == :mismatch
    end
  end

  describe 'matching' do
    it 'matches literals' do
      session = session_for('/foo/bar')
      session << '/foo/bar'
      session.finish.should be_matched
    end

    it 'does not match different literals' do
      session = session_for('/foo/bar')
      session << '/foo/baz'
      session.finish.should_not be_matched
    end

    it 'captures a single segment' do
      session = session_for('/:name')
      session << '/hello'
      session.finish.params.should be == { 'name' => 'hello' }
    end

    it 'captures multiple segments' do
      session = session_for('/:name/:id')
      session << '/hello/42'
      session.finish.params.should be == { 'name' => 'hello', 'id' => '42' }
    end

    it 'supports optional segments' do
      with_id = session_for('/:name(/:id)?')
      with_id << '/foo/1'
      with_id.finish.params.should be == { 'name' => 'foo', 'id' => '1' }

      without_id = session_for('/:name(/:id)?')
      without_id << '/foo'
      without_id.finish.params.should be == { 'name' => 'foo', 'id' => nil }
    end

    it 'supports optional captures' do
      session = session_for('/:name?')
      session << '/'
      session.finish.params.should be == { 'name' => nil }
    end

    it 'supports splats' do
      session = session_for('/*rest')
      session << '/a/b/c'
      session.finish.params.should be == { 'rest' => 'a/b/c' }
    end

    it 'supports unnamed splats' do
      session = session_for('/*')
      session << '/a/b'
      session.finish.params.should be == { 'splat' => ['a/b'] }
    end

    it 'supports splats followed by more segments' do
      session = session_for('/files/*path/edit')
      session << '/files/a/b/edit'
      session.finish.params.should be == { 'path' => 'a/b' }
    end

    it 'supports optional segments containing splats' do
      session = session_for('/:name(/*rest)?')
      session << '/a/b/c'
      session.finish.params.should be == { 'name' => 'a', 'rest' => 'b/c' }
    end

    it 'supports non-greedy captures' do
      session = session_for('/:a.:b', greedy: false)
      session << '/a.b.c'
      session.finish.params.should be == { 'a' => 'a', 'b' => 'b.c' }
    end

    it 'matches percent encoded literals' do
      session = session_for('/a b')
      session << '/a%20b'
      session.finish.should be_matched
    end

    it 'decodes percent encoded captures' do
      session = session_for('/:name')
      session << '/a%2Fb'
      session.finish.params.should be == { 'name' => 'a/b' }
    end

    it 'matches multibyte UTF-8 literals' do
      session = session_for('/ä/ö')
      session << '/ä/ö'
      session.finish.should be_matched
    end
  end

  describe 'chunk boundaries' do
    it 'allows percent escapes to span chunks' do
      session = session_for('/a b')
      (session << '/a%').should be == :continue
      (session << '20b').should be == :continue
      session.finish.should be_matched
    end

    it 'allows multibyte UTF-8 characters to span chunks' do
      session = session_for('/:name')
      session << "/\xE4".b
      session << "\xBD".b
      session << "\xA0".b
      session.finish.params.should be == { 'name' => '你' }
    end

    it 'produces the same params when feeding byte by byte or in one go' do
      {
        '/:name/:id'           => '/hello/42',
        '/:name(/:id)?/*rest'  => '/foo/1/a/b',
        '/:name(/*rest)?'      => '/a/b/c',
        '/files/*path/edit'    => '/files/a/b/edit',
        '/:name.:ext'          => '/a.b.c',
        '/ä/:x'                => '/ä/%E4%BD%A0',
        '/a b/:c'              => '/a%20b/x+y'
      }.each do |pattern_string, input|
        pattern  = Mustermann.new(pattern_string)
        expected = pattern.params(input)
        [:one_shot, :chars, :bytes, 2, 3, 7].each do |chunking|
          session = feed(Mustermann::Incremental.new(pattern), input, chunking)
          result  = session.finish
          expect(result.params).to be == expected,
            "#{pattern_string} vs #{input.inspect} fed #{chunking}: #{result.params.inspect} != #{expected.inspect}"
        end
      end
    end

    it 'agrees with Pattern#params for arbitrary two-way splits' do
      [
        '/foo/bar', '/:name', '/:name/:id', '/:name.:ext', '/:name(/:id)?',
        '/:name?', '/*', '/*rest', '/files/*path/edit', '/:name(/*rest)?',
        '/a b', '/ä/ö', '/foo(/bar)?(/baz)?', '/x/:a/:b?'
      ].each do |pattern_string|
        pattern = Mustermann.new(pattern_string)
        [
          '/foo/bar', '/foo', '/abc', '/a%20b', '/a+b', '/%E4%BD%A0', '/a%2Fb',
          '/a.b.c', '/foo/1', '/foo/1/2', '/x/y', '/files/a/b/edit', '/a b',
          '/ä/ö', '/%C3%A4/%C3%B6', '/foo/bar/baz', '/'
        ].each do |input|
          expected = pattern.params(input)
          (input.length + 1).times do |i|
            session = Mustermann::Incremental.new(pattern)
            session << input[0...i]
            session << input[i..]
            result = session.finish
            expect(result.params).to be == expected,
              "#{pattern_string} vs #{input.inspect} split at #{i}: #{result.params.inspect} != #{expected.inspect}"
          end
        end
      end
    end
  end

  describe 'incomplete input' do
    it 'raises IncompleteInputError for a truncated UTF-8 character at finish' do
      session = session_for('/:name')
      session << "/\xE4\xBD".b
      expect { session.finish }.to raise_error(Mustermann::Incremental::IncompleteInputError)
    end

    it 'raises IncompleteInputError for a truncated percent escape at finish' do
      session = session_for('/a b')
      session << '/a%2'
      expect { session.finish }.to raise_error(Mustermann::Incremental::IncompleteInputError)
    end

    it 'raises IncompleteInputError for a dangling percent sign at finish' do
      session = session_for('/a b')
      session << '/a%'
      expect { session.finish }.to raise_error(Mustermann::Incremental::IncompleteInputError)
    end

    it 'does not raise if a dangling percent sign is part of a successful match' do
      session = session_for('/:name')
      session << '/a%'
      session.finish.params.should be == { 'name' => 'a%' }
    end

    it 'raises InvalidInputError for bytes that are not valid UTF-8' do
      session = session_for('/:name')
      expect { session << "/a\xFFb".b }.to raise_error(Mustermann::Incremental::InvalidInputError)
    end
  end

  describe 'session lifecycle' do
    it 'raises when fed after finish' do
      session = session_for('/:name')
      session << '/foo'
      session.finish
      expect { session << '/bar' }.to raise_error(Mustermann::Incremental::SessionClosedError)
    end

    it 'returns the same result when finished twice' do
      session = session_for('/:name')
      session << '/foo'
      session.finish.should be_equal(session.finish)
    end

    it 'does not share mutable captures between sessions' do
      pattern = Mustermann.new('/:name/:id')
      first, second = Mustermann::Incremental.new(pattern), Mustermann::Incremental.new(pattern)
      first  << '/foo/'
      second << '/bar'
      first  << '1'
      second << '/2'
      first.finish.params.should  be == { 'name' => 'foo', 'id' => '1' }
      second.finish.params.should be == { 'name' => 'bar', 'id' => '2' }
    end
  end

  describe 'performance' do
    it 'processes each character only once, no matter how many chunks arrive' do
      pattern = Mustermann.new('/:a/:b/:c(/*rest)?')
      path    = '/alpha/beta/gamma' + '/segment' * 2_500
      session = Mustermann::Incremental.new(pattern)
      # feeding ~20k one-character chunks would take minutes if every chunk
      # triggered a rescan of the whole prefix; linear processing finishes
      # in well under a second
      path.each_char { |char| session << char }
      session.input_length.should be == path.length
      session.finish.should be_matched
    end
  end
end
