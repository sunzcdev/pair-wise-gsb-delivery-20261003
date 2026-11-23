require 'vcr/cassette/migrator'
require 'json'

RSpec.describe VCR::Cassette::Migrator::DirectoryMigration do
  let(:source_dir) { './tmp/directory_migration/source' }
  let(:target_dir) { './tmp/directory_migration/target' }
  let(:out_io)     { StringIO.new }
  let(:output)     { out_io.rewind; out_io.read }

  subject(:migration) { described_class.new(source_dir, target_dir, out_io) }

  # A body that is not valid UTF-8 and therefore cannot be stored as a plain
  # string by every serializer.
  let(:binary_body) { "\x89\x50\x4E\x47\x0D\x0A\x1A\x0A\x00\xFF\xFE".b }

  let(:old_yaml_contents) { <<-EOF }
---
- !ruby/struct:VCR::HTTPInteraction
  request: !ruby/struct:VCR::Request
    method: :get
    uri: http://example.com:80/image
    body:
    headers:
  response: !ruby/struct:VCR::Response
    status: !ruby/struct:VCR::ResponseStatus
      code: 200
      message: OK
    headers:
      content-type:
      - image/png
    body: !binary |
      #{[binary_body].pack('m0')}
    http_version: "1.1"
EOF

  let(:json_body) { "Héllo JSON" }

  let(:json_contents) do
    JSON.pretty_generate(
      "http_interactions" => [{
        "request" => {
          "method"  => "get",
          "uri"     => "http://example.com/json",
          "body"    => { "encoding" => "US-ASCII", "string" => "" },
          "headers" => {}
        },
        "response" => {
          "status"  => { "code" => 200, "message" => "OK" },
          "headers" => {},
          "body"    => { "encoding" => "UTF-8", "string" => json_body }
        },
        "recorded_at" => "Wed, 04 May 2011 12:30:00 GMT"
      }],
      "recorded_with" => "VCR #{VCR.version}"
    )
  end

  let(:unknown_contents) { "---\nrecorded_with: VCR 99.0\nhttp_interactions: not-an-array\n" }

  def cassette_contents(serializer_name, body)
    VCR.cassette_serializers[serializer_name].serialize(
      "http_interactions" => [{
        "request" => {
          "method"  => "get",
          "uri"     => "http://example.com/old",
          "body"    => { "encoding" => "US-ASCII", "string" => "" },
          "headers" => {}
        },
        "response" => {
          "status"  => { "code" => 200, "message" => "OK" },
          "headers" => {},
          "body"    => { "encoding" => "UTF-8", "string" => body }
        },
        "recorded_at" => "Tue, 03 May 2011 12:30:00 GMT"
      }],
      "recorded_with" => "VCR #{VCR.version}"
    )
  end

  def write_file(dir, relative_path, contents)
    path = File.join(dir, relative_path)
    FileUtils.mkdir_p(File.dirname(path))
    File.binwrite(path, contents)
  end

  def bodies_played_back_from(path, serializer_name)
    hash = VCR.cassette_serializers[serializer_name].deserialize(File.binread(path))
    hash['http_interactions'].map do |interaction_hash|
      interaction = VCR::HTTPInteraction.from_hash(interaction_hash)
      [interaction.request.body.to_s.b, interaction.response.body.to_s.b]
    end
  end

  def bytes_of(dir)
    Dir[File.join(dir, '**', '*')].select { |f| File.file?(f) }.sort.map do |file|
      [file.sub(%r|\A#{Regexp.escape(File.expand_path(dir))}/?|, ''), File.binread(file)]
    end.to_h
  end

  before(:each) do
    FileUtils.rm_rf './tmp/directory_migration'
    FileUtils.mkdir_p source_dir
    FileUtils.mkdir_p target_dir

    # Same-named cassettes in both directories: an old VCR 1.x YAML cassette
    # with a non-UTF-8 (binary) body, and a readable JSON cassette.
    write_file(source_dir, 'binary.yml', old_yaml_contents)
    write_file(source_dir, 'api/readable.json', json_contents)
    write_file(target_dir, 'binary.yml', cassette_contents(:yaml, 'OLD-BINARY'))
    write_file(target_dir, 'api/readable.json', cassette_contents(:json, 'OLD-JSON'))

    # A fixture in an unknown (future) format.
    write_file(source_dir, 'unknown.yml', unknown_contents)
  end

  def entry_for(relative_path)
    migration.entries.find { |e| e.relative_path == relative_path }
  end

  it 'reports the format and destination of each cassette by relative path without writing anything' do
    target_bytes_before = bytes_of(target_dir)

    migration.review

    expect(entry_for('binary.yml').format).to eq(:vcr_1_yaml)
    expect(entry_for('binary.yml').destination).to eq('binary.yml')
    expect(entry_for('api/readable.json').format).to eq(:json)
    expect(entry_for('api/readable.json').destination).to eq('api/readable.json')
    expect(entry_for('unknown.yml').format).to eq(:unknown)

    expect(output).to include('binary.yml: format=vcr_1_yaml, destination=binary.yml')
    expect(output).to include('api/readable.json: format=json, destination=api/readable.json')
    expect(output).to include('unknown.yml: format=unknown, skipped')

    expect(bytes_of(target_dir)).to eq(target_bytes_before)
  end

  context 'when a conversion fails after writing but before the re-read is confirmed' do
    def simulate_one_failure_before_reread_confirmation
      fail_once = true
      suffix = described_class::TEMP_FILE_SUFFIX
      allow(File).to receive(:binread).and_wrap_original do |original, path, *args|
        if fail_once && path.to_s.end_with?("binary.yml#{suffix}")
          fail_once = false
          raise Errno::EIO, 'simulated I/O failure'
        end
        original.call(path, *args)
      end
    end

    it 'keeps the old target replayable, reports the failed stage, and succeeds on a normal retry' do
      source_bytes_before = bytes_of(source_dir)
      migration.review
      simulate_one_failure_before_reread_confirmation

      # First publish: the binary cassette fails during verification.
      migration.publish!

      expect(entry_for('binary.yml').failed_at).to eq(:verify)
      expect(output).to match(/Failed binary\.yml during verify: Errno::EIO/)

      # The pre-existing target was not turned into a half-written file: it is
      # still the old, replayable cassette.
      expect(bodies_played_back_from(File.join(target_dir, 'binary.yml'), :yaml))
        .to eq([[''.b, 'OLD-BINARY'.b]])
      expect(Dir[File.join(target_dir, '**', "*#{described_class::TEMP_FILE_SUFFIX}*")]).to be_empty

      # The JSON cassette was migrated in the same round.
      expect(bodies_played_back_from(File.join(target_dir, 'api/readable.json'), :json))
        .to eq([[''.b, json_body.b]])

      # A normal retry migrates the binary cassette.
      migration.publish!

      expect(entry_for('binary.yml').failed_at).to be_nil
      expect(bodies_played_back_from(File.join(target_dir, 'binary.yml'), :yaml))
        .to eq([[''.b, binary_body]])
      expect(bodies_played_back_from(File.join(target_dir, 'api/readable.json'), :json))
        .to eq([[''.b, json_body.b]])

      # The unknown fixture was never published and nothing in the source
      # directory was modified.
      expect(File.exist?(File.join(target_dir, 'unknown.yml'))).to be(false)
      expect(bytes_of(source_dir)).to eq(source_bytes_before)
    end
  end

  it 'can be run multiple times, leaving already migrated cassettes unchanged' do
    migration.review
    migration.publish!
    target_bytes = bytes_of(target_dir)

    migration.review
    migration.publish!

    expect(output).to match(/Unchanged binary\.yml/)
    expect(output).to match(%r{Unchanged api/readable\.json})
    expect(bytes_of(target_dir)).to eq(target_bytes)
  end

  it 'migrates an old YAML cassette with a binary body through the target serializer, preserving the exact bytes' do
    migration.review
    migration.publish!

    hash = VCR.cassette_serializers[:yaml].deserialize(File.binread(File.join(target_dir, 'binary.yml')))
    interaction = hash['http_interactions'].first
    expect(interaction['request']['uri']).to eq('http://example.com/image')
    expect(interaction['response']['body']).to include('base64_string')
    expect(VCR::Response.body_from(interaction['response']['body']).b).to eq(binary_body)
  end

  it 'leaves unknown-format fixtures and all source files byte-for-byte intact' do
    source_bytes_before = bytes_of(source_dir)

    migration.review
    migration.publish!

    expect(output).to match(/Skipped unknown\.yml/)
    expect(File.binread(File.join(source_dir, 'unknown.yml'))).to eq(unknown_contents)
    expect(bytes_of(source_dir)).to eq(source_bytes_before)
  end

  describe 'Migrator.migrate_directory' do
    it 'reviews and then publishes the migration' do
      result = VCR::Cassette::Migrator.migrate_directory(source_dir, target_dir, out_io)

      expect(result.entries.map(&:relative_path)).to contain_exactly('binary.yml', 'api/readable.json', 'unknown.yml')
      expect(output).to include('Reviewing VCR cassettes')
      expect(output).to include('Publishing migrated cassettes')
      expect(bodies_played_back_from(File.join(target_dir, 'binary.yml'), :yaml))
        .to eq([[''.b, binary_body]])
    end
  end
end
