require 'tmpdir'
require 'fileutils'
require 'yaml'
require 'vcr'
require 'vcr/util/internet_connection'

# These specs deliberately do not require `spec_helper` so they can run in a
# minimal environment (no HTTP library stubs needed): everything here goes
# through the cassette API directly. The per-example setup below is applied
# after any global `before` hooks, so the file is also safe to run as part of
# the full suite.
RSpec.describe 'VCR::Cassette complete re-record mode' do
  def http_interaction(uri, body, recorded_at)
    request  = VCR::Request.new(:get, uri, nil, {})
    response = VCR::Response.new(VCR::ResponseStatus.new(200, 'OK'), {}, body)
    VCR::HTTPInteraction.new(request, response, recorded_at)
  end

  before(:each) do
    @dir = Dir.mktmpdir('vcr-complete-re-record')
    VCR.configure do |c|
      c.cassette_library_dir = @dir
      c.default_cassette_options = { match_requests_on: [:method, :uri] }
    end
  end

  after(:each) do
    FileUtils.remove_entry(@dir) if @dir && File.exist?(@dir)
  end

  let(:interval) { 7 * 24 * 60 * 60 }
  let(:cassette_path) { File.join(@dir, 'fixture.yml') }
  let(:pending_path)  { File.join(@dir, 'fixture-pending.yml') }
  let(:original_bytes) { File.binread(cassette_path) }

  def seed(interactions)
    File.write(cassette_path, YAML.dump(
      'http_interactions' => interactions.map(&:to_hash),
      'recorded_with'     => "VCR #{VCR.version}"
    ))
  end

  def aged(uri, body)
    http_interaction(uri, body, Time.now - interval - 60)
  end

  def body_of(hash)
    body = hash['response']['body']
    body.is_a?(Hash) ? body['string'] : body
  end

  def new_cassette(options = {})
    VCR::Cassette.new('fixture', options)
  end

  before(:each) do
    allow(VCR::InternetConnection).to receive(:available?).and_return(true)
    seed([aged('http://example.com/alpha', 'alpha old'),
          aged('http://example.com/beta',  'beta old')])
  end

  it 'defaults to :overwrite' do
    expect(VCR::Cassette.new('other').re_record_mode).to eq(:overwrite)
  end

  it 'rejects an unknown re-record mode' do
    expect { VCR::Cassette.new('other', re_record_mode: :sometimes) }
      .to raise_error(ArgumentError, /not a valid cassette re-record mode/)
  end

  it 'only re-records when the interval has elapsed' do
    cassette = VCR::Cassette.new('other', re_record_mode: :complete)
    expect(cassette.re_recording?).to be false
    expect(cassette.complete_re_record?).to be false

    cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete)
    expect(cassette.re_recording?).to be true
    expect(cassette.complete_re_record?).to be true
    expect(cassette.record_mode).to eq(:all)
  end

  it 'leaves :overwrite behaviour untouched' do
    cassette = new_cassette(re_record_interval: interval)
    cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
    cassette.eject

    expect(File).not_to exist(pending_path)

    saved = YAML.load_file(cassette_path)['http_interactions']
    expect(saved.map { |i| i['request']['uri'] }).to eq(['http://example.com/beta', 'http://example.com/alpha'])
    # The stale interaction is rewritten with its old timestamp and there is no
    # record of anything having been missed -- that is exactly the silent
    # surface refresh that :complete mode is meant to avoid.
    beta = saved.find { |i| i['request']['uri'].end_with?('/beta') }
    expect(Time.httpdate(beta['recorded_at'])).to be < Time.now - interval
    expect(cassette.re_record_diagnostics).to be_nil
  end

  it 'replaces the cassette when every interaction was recorded again' do
    File.write(pending_path, YAML.dump('http_interactions' => [], 're_record_diagnostics' => { 'status' => 'incomplete' }))

    cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete)
    cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
    cassette.record_http_interaction(http_interaction('http://example.com/beta',  'beta new',  Time.now))
    cassette.eject

    expect(YAML.load_file(cassette_path)['http_interactions'].map { |i| body_of(i) })
      .to eq(['alpha new', 'beta new'])
    expect(File).not_to exist(pending_path)
    expect(cassette.re_record_diagnostics).to include(
      'status' => 'complete', 'missing_interaction_count' => 0
    )
  end

  context 'when only one of two interactions was recorded again' do
    let!(:cassette) do
      c = new_cassette(re_record_interval: interval, re_record_mode: :complete)
      c.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
      c.eject
      c
    end

    it 'leaves the previous cassette byte-for-byte unchanged' do
      expect(original_bytes).to eq(File.binread(cassette_path))
    end

    it 'does not silently refresh the recorded_at of the stale interaction' do
      beta = YAML.load_file(cassette_path)['http_interactions']
             .find { |i| i['request']['uri'].end_with?('/beta') }
      expect(Time.httpdate(beta['recorded_at'])).to be < Time.now - interval
    end

    it 'records the missing interaction as diagnostics in a sibling file' do
      staged = YAML.load_file(pending_path)
      expect(staged['http_interactions'].map { |i| body_of(i) }).to eq(['alpha new'])

      diag = staged['re_record_diagnostics']
      expect(diag['status']).to eq('incomplete')
      expect(diag['expected_interaction_count']).to eq(2)
      expect(diag['missing_interaction_count']).to eq(1)
      expect(diag['missing_interactions'].first['uri']).to eq('http://example.com/beta')
      expect(diag['previous_cassette_preserved']).to be true
    end

    it 'exposes diagnostics through the cassette' do
      expect(cassette.re_record_diagnostics['missing_interactions'].map { |i| i['uri'] })
        .to eq(['http://example.com/beta'])
    end

    it 'stays re-recordable on the next run' do
      next_run = new_cassette(re_record_interval: interval, re_record_mode: :complete)
      expect(next_run).to be_complete_re_record
    end
  end

  it 'keeps the cassette and persists nothing when record_on_error is false and the run failed' do
    cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete, record_on_error: false)
    cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
    cassette.run_failed!
    cassette.eject

    expect(File.binread(cassette_path)).to eq(original_bytes)
    expect(File).not_to exist(pending_path)
    expect(cassette.re_record_diagnostics['status']).to eq('aborted')
  end

  it 'requires one fresh recording per duplicate occurrence' do
    seed([aged('http://example.com/dup', 'dup old 1'),
          aged('http://example.com/dup', 'dup old 2')])

    cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete)
    cassette.record_http_interaction(http_interaction('http://example.com/dup', 'dup new 1', Time.now))
    cassette.eject

    expect(cassette.re_record_diagnostics['status']).to eq('incomplete')
    expect(cassette.re_record_diagnostics['missing_interaction_count']).to eq(1)
  end

  def with_before_record_hook(hook)
    original = VCR.configuration.hooks[:before_record].dup
    VCR.configuration.before_record(&hook)
    yield
  ensure
    VCR.configuration.hooks[:before_record] = original
  end

  it 'does not count interactions dropped by a before_record hook as refreshed' do
    hook = lambda { |i| i.ignore! if i.request.uri.end_with?('/beta') }
    with_before_record_hook(hook) do
      cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete)
      cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
      cassette.record_http_interaction(http_interaction('http://example.com/beta',  'beta new',  Time.now))
      cassette.eject

      expect(cassette.re_record_diagnostics['status']).to eq('incomplete')
      expect(cassette.re_record_diagnostics['missing_interactions'].map { |i| i['uri'] })
        .to eq(['http://example.com/beta'])
      expect(File.binread(cassette_path)).to eq(original_bytes)
    end
  end

  it 'keeps the previous cassette when a before_record hook drops every interaction' do
    with_before_record_hook(lambda { |i| i.ignore! }) do
      cassette = new_cassette(re_record_interval: interval, re_record_mode: :complete)
      cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
      cassette.record_http_interaction(http_interaction('http://example.com/beta',  'beta new',  Time.now))
      cassette.eject

      expect(File.binread(cassette_path)).to eq(original_bytes)
      # Nothing was actually persisted, so the attempt is still incomplete and
      # both interactions are reported as missing.
      expect(cassette.re_record_diagnostics['status']).to eq('incomplete')
      expect(cassette.re_record_diagnostics['missing_interaction_count']).to eq(2)
      expect(YAML.load_file(pending_path)['http_interactions']).to be_empty
    end
  end
  it 'works with a persister that only implements [] and []=' do
    store = { 'fixture.yml' => File.binread(cassette_path) }
    persister = Object.new
    persister.define_singleton_method(:[]) { |key| store[key] }
    persister.define_singleton_method(:[]=) { |key, value| store[key] = value }

    VCR.cassette_persisters[:in_memory_for_spec] = persister

    cassette = VCR::Cassette.new('fixture',
      persist_with: :in_memory_for_spec,
      match_requests_on: [:method, :uri],
      re_record_interval: interval,
      re_record_mode: :complete)

    cassette.record_http_interaction(http_interaction('http://example.com/alpha', 'alpha new', Time.now))
    cassette.eject

    # The old cassette is untouched and the partial attempt is staged.
    expect(store['fixture.yml']).to eq(original_bytes)
    expect(store).to have_key('fixture-pending.yml')
    expect(cassette.re_record_diagnostics['status']).to eq('incomplete')
  end

  describe 'VCR::Cassette::Persisters::FileSystem#replace' do
    let(:persister) { VCR::Cassette::Persisters::FileSystem }

    it 'writes the content and leaves no temporary files behind' do
      persister['atomic.yml'] = 'before'
      persister.replace('atomic.yml', 'after')

      expect(persister['atomic.yml']).to eq('after')
      expect(Dir[File.join(@dir, '*.tmp')]).to be_empty
    end

    it 'creates intermediate directories' do
      persister.replace('nested/dir/atomic.yml', 'value')
      expect(File.read(File.join(@dir, 'nested/dir/atomic.yml'))).to eq('value')
    end

    it 'deletes files' do
      persister['gone.yml'] = 'value'
      expect(persister.delete('gone.yml')).to be true
      expect(persister['gone.yml']).to be_nil
      expect(persister.delete('gone.yml')).to be false
    end
  end
end


