require 'spec_helper'
require 'vcr/cassette/persisters/safe_writer'

RSpec.describe VCR::Cassette::Persisters::SafeWriter do
  let(:tmp_dir) { Dir.mktmpdir }
  let(:path) { File.join(tmp_dir, 'example.yml') }
  let(:lock_path) { "#{path}.lock" }
  let(:serializer) { VCR.cassette_serializers[:yaml] }
  let(:matchers) { [:method, :uri] }

  after { FileUtils.rm_rf(tmp_dir) }

  def interaction(uri, body, method: :get)
    request = VCR::Request.new(method, uri, '', {})
    status = VCR::ResponseStatus.new(200, 'OK')
    response = VCR::Response.new(status, { 'Content-Length' => [body.bytesize.to_s] }, body, '1.1')
    VCR::HTTPInteraction.new(request, response)
  end

  def read_interactions
    return [] unless File.exist?(path)
    ::YAML.unsafe_load_file(path)['http_interactions']
  end

  def full_hash(interactions)
    { 'http_interactions' => interactions.map(&:to_hash), 'recorded_with' => "VCR #{VCR.version}" }
  end

  def interactions_in(bytes)
    return [] if bytes.to_s.empty?
    serializer.deserialize(bytes)['http_interactions'].map { |h| VCR::HTTPInteraction.from_hash(h) }
  end

  def writer(incoming, base_bytes:)
    described_class.new(
      path: path,
      serializer: serializer,
      incoming_interactions: incoming,
      base_bytes: base_bytes,
      request_matchers: matchers,
      full_cassette_hash: full_hash(interactions_in(base_bytes) + incoming)
    )
  end

  describe '#commit' do
    context 'when no other writer has touched the cassette' do
      it 'creates the cassette when it did not exist' do
        incoming = [interaction('http://example.com/a', 'AAA')]
        writer(incoming, base_bytes: nil).commit

        expect(read_interactions.map { |h| h['request']['uri'] }).to eq(['http://example.com/a'])
      end

      it 'writes through a temporary file and leaves no temp files behind' do
        incoming = [interaction('http://example.com/a', 'AAA')]
        writer(incoming, base_bytes: nil).commit

        expect(Dir.glob(File.join(tmp_dir, '**', '*.tmp'))).to be_empty
      end

      it 'creates parent directories as needed' do
        nested = File.join(tmp_dir, 'sub', 'dir', 'example.yml')
        incoming = [interaction('http://example.com/a', 'AAA')]
        described_class.new(
          path: nested, serializer: serializer, incoming_interactions: incoming,
          base_bytes: nil, request_matchers: matchers, full_cassette_hash: full_hash(incoming)
        ).commit

        expect(File).to exist(nested)
      end

      it 'keeps the single-process cassette hash exactly as provided' do
        base = [interaction('http://example.com/base', 'BASE')]
        File.binwrite(path, serializer.serialize(full_hash(base)))
        base_bytes = File.binread(path)
        incoming = [interaction('http://example.com/a', 'AAA')]

        writer(incoming, base_bytes: base_bytes).commit

        expect(read_interactions.map { |h| h['request']['uri'] })
          .to eq(['http://example.com/base', 'http://example.com/a'])
      end
    end

    context 'when another writer committed disjoint interactions in the meantime' do
      it 'merges the foreign additions with the incoming interactions' do
        foreign = [interaction('http://example.com/foreign', 'FOREIGN')]
        File.binwrite(path, serializer.serialize(full_hash(foreign)))

        incoming = [interaction('http://example.com/mine', 'MINE')]
        writer(incoming, base_bytes: nil).commit

        expect(read_interactions.map { |h| h['request']['uri'] }.sort)
          .to eq(['http://example.com/foreign', 'http://example.com/mine'])
      end

      it 'preserves interactions from the shared base revision alongside both additions' do
        base = [interaction('http://example.com/base', 'BASE')]
        File.binwrite(path, serializer.serialize(full_hash(base)))
        base_bytes = File.binread(path)

        foreign = base + [interaction('http://example.com/foreign', 'FOREIGN')]
        File.binwrite(path, serializer.serialize(full_hash(foreign)))

        incoming = [interaction('http://example.com/mine', 'MINE')]
        writer(incoming, base_bytes: base_bytes).commit

        expect(read_interactions.map { |h| h['request']['uri'] }.sort).to eq(
          ['http://example.com/base', 'http://example.com/foreign', 'http://example.com/mine']
        )
      end

      it 'keeps the on-disk revision metadata and recorded_with keys on merge' do
        foreign_hash = full_hash([interaction('http://example.com/foreign', 'FOREIGN')]).merge(
          'metadata' => { 'owner' => 'foreign-team' }
        )
        File.binwrite(path, serializer.serialize(foreign_hash))

        writer([interaction('http://example.com/mine', 'MINE')], base_bytes: nil).commit

        saved = ::YAML.unsafe_load_file(path)
        expect(saved['metadata']).to eq('owner' => 'foreign-team')
        expect(saved['recorded_with']).to eq("VCR #{VCR.version}")
      end
    end

    context 'when both writers recorded the same request with the same response' do
      it 'keeps both interactions without raising' do
        shared = interaction('http://example.com/shared', 'SAME')
        File.binwrite(path, serializer.serialize(full_hash([shared])))

        expect { writer([shared], base_bytes: nil).commit }.not_to raise_error
        expect(read_interactions.size).to eq(2)
      end
    end

    context 'when another writer recorded a different response for the same request' do
      let(:foreign) { interaction('http://example.com/shared', 'FOREIGN-BODY') }
      let(:mine) { interaction('http://example.com/shared', 'MINE-BODY') }

      before do
        File.binwrite(path, serializer.serialize(full_hash([foreign])))
      end

      it 'raises a CassetteWriteConflictError' do
        expect { writer([mine], base_bytes: nil).commit }
          .to raise_error(VCR::Errors::CassetteWriteConflictError)
      end

      it 'leaves the first-finished cassette untouched' do
        before = File.binread(path)
        expect { writer([mine], base_bytes: nil).commit }.to raise_error(VCR::Errors::CassetteWriteConflictError)
        expect(File.binread(path)).to eq(before)
      end

      it 'exposes diagnostic details on the error' do
        error = begin
          writer([mine], base_bytes: nil).commit
        rescue VCR::Errors::CassetteWriteConflictError => e
          e
        end

        expect(error.cassette_file).to eq(path)
        expect(error.conflicts.size).to eq(1)
        expect(error.conflicts.first[:request_summary]).to include('http://example.com/shared')
        expect(error.conflicts.first[:kept_response_summary]).to include('FOREIGN-BODY')
        expect(error.conflicts.first[:rejected_response_summary]).to include('MINE-BODY')
        expect(error.message).to include('FOREIGN-BODY', 'MINE-BODY', path)
      end

      it 'writes the rejected interactions to a sibling file' do
        error = begin
          writer([mine], base_bytes: nil).commit
        rescue VCR::Errors::CassetteWriteConflictError => e
          e
        end

        expect(File).to exist(error.rejected_interactions_file)
        rejected = ::YAML.unsafe_load_file(error.rejected_interactions_file)
        expect(rejected['http_interactions'].first['response']['body']['string']).to eq('MINE-BODY')
        expect(rejected['metadata']['reason']).to include('conflicting response')
      end

      it 'does not partially commit a batch containing one conflicting interaction' do
        disjoint = interaction('http://example.com/new', 'NEW')
        before = File.binread(path)

        expect { writer([mine, disjoint], base_bytes: nil).commit }
          .to raise_error(VCR::Errors::CassetteWriteConflictError)

        expect(File.binread(path)).to eq(before)
        expect(read_interactions.map { |h| h['request']['uri'] }).not_to include('http://example.com/new')
      end
    end

    context 'with multiple threads writing concurrently' do
      it 'serializes commits and keeps every disjoint interaction exactly once' do
        threads = 8.times.map do |i|
          Thread.new do
            incoming = [interaction("http://example.com/r#{i}", "body-#{i}")]
            # reuse the same empty base to mimic processes started together
            writer(incoming, base_bytes: nil).commit
          end
        end
        threads.each(&:join)

        uris = read_interactions.map { |h| h['request']['uri'] }
        expect(uris.uniq).to match_array(8.times.map { |i| "http://example.com/r#{i}" })
      end

      it 'lets only the first different response win and rejects the rest with conflicts' do
        errors = []
        threads = 4.times.map do |i|
          Thread.new do
            incoming = [interaction('http://example.com/same', "resp-#{i}")]
            begin
              writer(incoming, base_bytes: nil).commit
            rescue VCR::Errors::CassetteWriteConflictError => e
              errors << e
            end
          end
        end
        threads.each(&:join)

        expect(read_interactions.size).to eq(1)
        expect(errors.size).to eq(3)
        expect(errors.map(&:rejected_interactions_file).uniq.size).to eq(3)
      end
    end

    it 'holds an exclusive lock file next to the cassette for the read/merge/write section' do
      incoming = [interaction('http://example.com/a', 'AAA')]
      writer(incoming, base_bytes: nil).commit
      expect(File).to exist(lock_path)
    end

    it 'raises an InvalidCassetteFormatError when the on-disk cassette cannot be parsed' do
      File.binwrite(path, '--- not a vcr cassette')
      incoming = [interaction('http://example.com/a', 'AAA')]

      expect { writer(incoming, base_bytes: '').commit }
        .to raise_error(VCR::Errors::InvalidCassetteFormatError, /cannot be safely merged/)
    end
  end
end
