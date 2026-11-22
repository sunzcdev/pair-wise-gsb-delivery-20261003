require 'spec_helper'
require 'vcr/cassette/persisters/safe_writer'

# End-to-end check of the split-recording scenario: two independent Ruby
# processes start from the same cassette version and commit through the
# explicit safe-save entry point.
RSpec.describe 'safe cassette writes across processes', :skip_vcr_reset do
  let(:tmp_dir) { Dir.mktmpdir }
  let(:helper) { File.join(tmp_dir, 'process_writer.rb') }

  before do
    File.binwrite(helper, <<~RUBY)
      $LOAD_PATH.unshift(#{File.expand_path('lib').inspect})
      require 'vcr'

      mode, dir, cassette, label = ARGV
      VCR.configure do |c|
        c.cassette_library_dir = dir
      end

      request = VCR::Request.new(:get, "http://example.com/\#{label}", '', {})
      response = VCR::Response.new(
        VCR::ResponseStatus.new(200, 'OK'), {}, "body-\#{label}", '1.1'
      )
      incoming = [VCR::HTTPInteraction.new(request, response)]
      serializer = VCR.cassette_serializers[:yaml]

      if mode == 'conflict'
        request = VCR::Request.new(:get, 'http://example.com/shared', '', {})
        response = VCR::Response.new(
          VCR::ResponseStatus.new(200, 'OK'), {}, "response-\#{label}", '1.1'
        )
        incoming = [VCR::HTTPInteraction.new(request, response)]
      end

      VCR::Cassette::Persisters::SafeWriter.new(
        path: VCR::Cassette::Persisters::FileSystem.absolute_path_to_file("\#{cassette}.yml"),
        serializer: serializer,
        incoming_interactions: incoming,
        base_bytes: nil,
        request_matchers: [:method, :uri],
        full_cassette_hash: {
          'http_interactions' => incoming.map(&:to_hash),
          'recorded_with' => "VCR \#{VCR.version}"
        }
      ).commit
    RUBY
  end

  after { FileUtils.rm_rf(tmp_dir) }

  def run_writer(mode, cassette, label)
    spawn(Gem.ruby, helper, mode, tmp_dir, cassette, label,
          out: File.join(tmp_dir, "#{label}.out"), err: File.join(tmp_dir, "#{label}.err"))
  end

  def interactions_in(cassette)
    path = File.join(tmp_dir, "#{cassette}.yml")
    ::YAML.unsafe_load_file(path)['http_interactions']
  end

  it 'merges disjoint interactions committed from two processes' do
    first = run_writer('disjoint', 'merged', 'alpha')
    second = run_writer('disjoint', 'merged', 'beta')
    Process.wait(first)
    Process.wait(second)

    expect($?.exitstatus).to eq(0)
    uris = interactions_in('merged').map { |h| h['request']['uri'] }
    expect(uris.sort).to eq(['http://example.com/alpha', 'http://example.com/beta'])
  end

  it 'keeps the first finished response and rejects the conflicting one' do
    first = run_writer('conflict', 'clash', 'one')
    second = run_writer('conflict', 'clash', 'two')
    Process.wait(first)
    first_status = $?.exitstatus
    Process.wait(second)
    second_status = $?.exitstatus

    expect([first_status, second_status].sort).to eq([0, 1])
    expect(interactions_in('clash').size).to eq(1)

    rejected = Dir.glob(File.join(tmp_dir, 'clash.rejected-*.yml'))
    expect(rejected.size).to eq(1)
    rejected_interactions = ::YAML.unsafe_load_file(rejected.first)['http_interactions']
    expect(rejected_interactions.size).to eq(1)

    kept_body = interactions_in('clash').first['response']['body']['string']
    rejected_body = rejected_interactions.first['response']['body']['string']
    expect([kept_body, rejected_body].sort).to eq(['response-one', 'response-two'])
  end
end
