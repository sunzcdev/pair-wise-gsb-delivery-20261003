# frozen_string_literal: true
require 'support'
require 'mustermann'
require 'mustermann/sinatra'
require 'mustermann/regular'
require 'mustermann/identity'
require 'mustermann/incremental'

describe Mustermann::Incremental do
  def session_for(pattern_string, **options)
    described_class.new(Mustermann::Sinatra.new(pattern_string, **options))
  end

  describe 'feeding chunks' do
    it 'reports :continue while the input can still be extended to a match' do
      session = session_for('/:user/files/:name')
      expect(session.feed('/ann')).to be_continue
      expect(session.feed('a/files/')).to be_continue
      expect(session.feed('x')).to be_continue
      expect(session).to be_continue
      expect(session.status).to be == :continue
    end

    it 'reports :mismatch as soon as the input can never match' do
      session = session_for('/foo/:bar')
      expect(session.feed('/fo')).to be_continue
      expect(session.feed('x')).to be_mismatch
      expect(session).to be_mismatch
      expect(session.status).to be == :mismatch
    end

    it 'keeps reporting :mismatch once the input has diverged' do
      session = session_for('/foo/:bar')
      session.feed('/fox')
      expect(session.feed('/foo/bar')).to be_mismatch
      expect(session.feed('/foo/bar')).to be_mismatch
      expect(session.finish).to be_mismatch
    end

    it 'supports << for chaining' do
      session = session_for('/foo/:bar')
      expect(session << '/fo' << 'o/' << 'x').to be(session)
      expect(session.finish).to be_match
    end

    it 'handles empty chunks' do
      session = session_for('/foo')
      expect(session.feed('')).to be_continue
      expect(session.feed('/foo')).to be_continue
      expect(session.finish).to be_match
    end
  end

  describe 'finishing' do
    it 'reports :match with params for a complete match' do
      session = session_for('/:user/files/:name')
      session.feed('/anna/files/x')
      result = session.finish
      expect(result).to be_match
      expect(result.status).to be == :match
      expect(result.params).to be == { 'user' => 'anna', 'name' => 'x' }
      expect(session.status).to be == :match
    end

    it 'reports :incomplete when the input ends in the middle of the pattern' do
      session = session_for('/foo/:bar')
      session.feed('/foo')
      result = session.finish
      expect(result).to be_incomplete
      expect(result.params).to be_nil
    end

    it 'reports :incomplete for a dangling percent escape' do
      session = session_for('/foo')
      expect(session.feed('/%6')).to be_continue
      expect(session.finish).to be_incomplete
    end

    it 'reports :incomplete when a percent escape is cut off right before the end' do
      session = session_for('/foo')
      session.feed('/%66o')
      expect(session.finish).to be_incomplete
    end

    it 'reports :incomplete for a truncated UTF-8 character' do
      session = session_for('/:name')
      session.feed("/a\xC3".b)
      expect(session.finish).to be_incomplete
    end

    it 'reports :incomplete for a truncated UTF-8 character inside a literal' do
      session = session_for("/caf\u{e9}")
      expect(session.feed("/caf\xC3".b)).to be_continue
      expect(session.finish).to be_incomplete
    end

    it 'reports :mismatch for invalid bytes that are not a truncated character' do
      session = session_for('/:name')
      session.feed("/a\xFF".b)
      expect(session.finish).to be_mismatch
    end

    it 'is idempotent' do
      session = session_for('/:a')
      session.feed('/x')
      expect(session.finish).to be(session.finish)
    end

    it 'raises when feeding a finished session' do
      session = session_for('/:a')
      session.feed('/x')
      session.finish
      expect { session.feed('/y') }.to raise_error(Mustermann::Error, /finished/)
    end
  end

  describe 'supported pattern features' do
    it 'matches named captures' do
      session = session_for('/:a/:b')
      session.feed('/foo/bar')
      expect(session.finish.params).to be == { 'a' => 'foo', 'b' => 'bar' }
    end

    it 'matches optional segments' do
      pattern = Mustermann::Sinatra.new('/:a/?:b?')

      session = described_class.new(pattern)
      session.feed('/x')
      expect(session.finish.params).to be == pattern.params('/x')

      session = described_class.new(pattern)
      session.feed('/x/y')
      expect(session.finish.params).to be == pattern.params('/x/y')
    end

    it 'matches optional segments spanning chunk boundaries' do
      session = session_for('/:a/?:b?')
      expect(session.feed('/x')).to be_continue
      expect(session.feed('/')).to be_continue
      expect(session.feed('y')).to be_continue
      expect(session.finish.params).to be == { 'a' => 'x', 'b' => 'y' }
    end

    it 'matches splats, including slashes' do
      session = session_for('/foo/*splat')
      session.feed('/foo/a/b/c')
      expect(session.finish.params).to be == Mustermann::Sinatra.new('/foo/*splat').params('/foo/a/b/c')
    end

    it 'matches multiple splats' do
      pattern = Mustermann::Sinatra.new('/download/*.*')
      session = described_class.new(pattern)
      session.feed('/download/a.tar.gz')
      expect(session.finish.params).to be == pattern.params('/download/a.tar.gz')
    end

    it 'matches an empty splat' do
      session = session_for('/foo/*')
      session.feed('/foo/')
      expect(session.finish).to be_match
    end

    it 'does not let splats match newlines' do
      session = session_for('/foo/*')
      expect(session.feed("/foo/a\nb")).to be_mismatch
    end

    it 'matches adjacent captures and literals' do
      session = session_for('/:a.:b')
      session.feed('/x.y')
      expect(session.finish.params).to be == { 'a' => 'x', 'b' => 'y' }
    end

    it 'matches percent-encoded literals' do
      session = session_for("/caf\u{e9}")
      expect(session.feed('/caf')).to be_continue
      expect(session.feed('%C3')).to be_continue
      expect(session.feed('%A9')).to be_continue
      expect(session.finish).to be_match
    end

    it 'matches lower case percent-encoded literals' do
      session = session_for("/caf\u{e9}")
      session.feed('/caf%c3%a9')
      expect(session.finish).to be_match
    end

    it 'matches UTF-8 literals fed as raw bytes split across chunks' do
      session = session_for('/:name')
      session.feed("/caf\xC3".b)
      expect(session.feed("\xA9".b)).to be_continue
      expect(session.finish.params).to be == { 'name' => "caf\u{e9}" }
    end

    it 'matches space as plus by default' do
      session = session_for('/a b')
      session.feed('/a+b')
      expect(session.finish).to be_match
    end

    it 'does not match space as plus with space_matches_plus: false' do
      session = session_for('/a b', space_matches_plus: false)
      expect(session.feed('/a+')).to be_mismatch
    end

    it 'does not match percent escapes with uri_decode: false' do
      session = session_for("/caf\u{e9}", uri_decode: false)
      expect(session.feed('/caf%')).to be_mismatch
    end

    it 'matches the empty pattern' do
      session = session_for('')
      expect(session.finish.params).to be == {}
      expect(session_for('').feed('x')).to be_mismatch
    end
  end

  describe 'byte-wise and one-shot equivalence' do
    examples = [
      ['/:a/:b',        '/foo/bar'],
      ['/:a/?:b?',      '/foo'],
      ['/:a/?:b?',      '/foo/bar'],
      ['/foo/*splat',   '/foo/a/b/c'],
      ['/download/*.*', '/download/a.tar.gz'],
      ["/caf\u{e9}",    '/caf%C3%A9'],
      ["/caf\u{e9}",    "/caf\u{e9}"],
      ['/:name',        '/caf%C3%A9'],
      ['/a b',          '/a+b'],
      ['/a b',          '/a%20b'],
      ['/:a.:b',        '/x.y'],
      ['/:a/:b',        '/foo'],
      ['/foo/:bar',     '/fox']
    ]

    examples.each do |pattern_string, path|
      it "feeding #{path.inspect} to #{pattern_string.inspect} byte by byte, in chunks and at once behaves the same" do
        pattern  = Mustermann::Sinatra.new(pattern_string)
        at_once  = described_class.new(pattern)
        chunked  = described_class.new(pattern)
        bytewise = described_class.new(pattern)

        at_once.feed(path)
        path.scan(/.{1,3}/m) { |slice| chunked.feed(slice) }
        path.each_byte { |byte| bytewise.feed(byte.chr) }

        expect(chunked.finish.status).to  be == at_once.finish.status
        expect(bytewise.finish.status).to be == at_once.finish.status
        expect(chunked.finish.params).to  be == at_once.finish.params
        expect(bytewise.finish.params).to be == at_once.finish.params
        expect(at_once.finish.params).to  be == pattern.params(path)
      end
    end
  end

  describe 'session isolation' do
    it 'does not share state between sessions of the same pattern' do
      pattern = Mustermann::Sinatra.new('/:a')
      first   = described_class.new(pattern)
      second  = described_class.new(pattern)

      first.feed('/foo')
      second.feed('/bar')

      expect(first.finish.params).to  be == { 'a' => 'foo' }
      expect(second.finish.params).to be == { 'a' => 'bar' }
    end

    it 'does not share captures between sessions' do
      pattern = Mustermann::Sinatra.new('/:a')
      first   = described_class.new(pattern)
      second  = described_class.new(pattern)
      first.feed('/foo')
      second.feed('/bar')

      first.finish.params['a'] << ' mutated'
      expect(second.finish.params).to be == { 'a' => 'bar' }
      expect(pattern.params('/foo')).to be == { 'a' => 'foo' }
    end

    it 'does not let a mismatch in one session affect another' do
      pattern = Mustermann::Sinatra.new('/foo/:a')
      first   = described_class.new(pattern)
      second  = described_class.new(pattern)

      expect(first.feed('/fox')).to be_mismatch
      expect(second.feed('/foo/x')).to be_continue
      expect(second.finish).to be_match
    end
  end

  describe 'incremental work' do
    it 'does not invoke the pattern regexp while feeding' do
      pattern = Mustermann::Sinatra.new('/:line')
      session = described_class.new(pattern)
      expect(pattern).not_to receive(:match)
      expect(pattern).not_to receive(:params)
      expect(pattern).not_to receive(:===)
      expect(session.feed('/')).to be_continue
      1_000.times { expect(session.feed('a')).to be_continue }
    end

    it 'extracts params via the pattern exactly once, at finish' do
      pattern = Mustermann::Sinatra.new('/:line')
      session = described_class.new(pattern)
      session.feed('/abc')
      expect(pattern).to receive(:params).once.and_call_original
      session.finish
      session.finish
    end

    it 'handles many small chunks without rescanning the prefix' do
      session = session_for('/:line')
      session.feed('/')
      20_000.times { session.feed('a') }
      expect(session.finish.params).to be == { 'line' => 'a' * 20_000 }
    end
  end

  describe 'unsupported patterns' do
    it 'rejects patterns with look-aheads' do
      expect { session_for('/:a(.:ext)?') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError, /look_ahead/)
    end

    it 'rejects patterns with unions' do
      expect { session_for('/(a|b)') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError, /union/)
    end

    it 'rejects the :capture option' do
      expect { session_for('/:a', capture: :word) }.to raise_error(Mustermann::Incremental::UnsupportedPatternError, /:capture/)
    end

    it 'rejects the :except option' do
      expect { session_for('/:a', except: '/foo') }.to raise_error(Mustermann::Incremental::UnsupportedPatternError, /:except/)
    end

    it 'rejects non-AST patterns' do
      expect { described_class.new(Mustermann::Regular.new('.*')) }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
      expect { described_class.new(Mustermann::Identity.new('/foo')) }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
      expect { described_class.new(Mustermann.new(/foo/)) }.to raise_error(Mustermann::Incremental::UnsupportedPatternError)
    end

    it 'rejects non-pattern input to Session' do
      expect { Mustermann::Incremental::Session.new('/:a') }.to raise_error(TypeError)
    end
  end

  describe 'entry points' do
    it 'creates a session from a pattern string' do
      session = described_class.new('/:a')
      session.feed('/x')
      expect(session.finish.params).to be == { 'a' => 'x' }
    end

    it 'passes options through for pattern strings' do
      session = described_class.new('/a b', space_matches_plus: false)
      expect(session.feed('/a+')).to be_mismatch
    end

    it 'creates a session via Pattern#incremental' do
      session = Mustermann::Sinatra.new('/:a').incremental
      expect(session).to be_a(Mustermann::Incremental::Session)
      session.feed('/x')
      expect(session.finish).to be_match
    end
  end

  describe Mustermann::Incremental::Result do
    it 'exposes status and params' do
      result = Mustermann::Incremental::Result.new(:match, params: { 'a' => 'b' })
      expect(result.status).to be == :match
      expect(result.params).to be == { 'a' => 'b' }
      expect(result).to be_match
      expect(result).not_to be_continue
      expect(result).not_to be_mismatch
      expect(result).not_to be_incomplete
      expect(result.inspect).to be_include(':match')
    end
  end
end
