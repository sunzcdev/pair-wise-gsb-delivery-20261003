require 'vcr'
require 'yaml'
require 'tmpdir'

# End-to-end coverage for playback groups: parallel tests share a single
# cassette file, and the same request represents a different playback
# sequence in each test. Group A records 202, 200 and group B records
# 503, 204 at the same address; two interleaved threads then replay
# A, B, B, A and get 202, 503, 204, 200.
RSpec.describe 'Playback groups' do
  let(:uri) { 'http://example.com/playback-groups' }

  around(:each) do |example|
    Dir.mktmpdir do |dir|
      VCR.configuration.cassette_library_dir = dir
      example.run
    end
  end

  def build_interaction(status, body)
    VCR::HTTPInteraction.new(
      VCR::Request.new(:get, uri, '', {}),
      VCR::Response.new(VCR::ResponseStatus.new(status, ''), {}, body)
    )
  end

  def record_group(group, statuses)
    VCR.use_cassette('shared', record: :new_episodes, playback_group: group) do |cassette|
      statuses.each do |status|
        cassette.record_http_interaction(build_interaction(status, "recorded #{status}"))
      end
    end
  end

  let(:request) { VCR::Request.new(:get, uri, '', {}) }

  # Record, at the same address, group A's 202 and 200 and group B's 503 and 204.
  before do
    record_group('a', [202, 200])
    record_group('b', [503, 204])
  end

  def recorded_interaction_hashes
    file = File.join(VCR.configuration.cassette_library_dir, 'shared.yml')
    YAML.load_file(file)['http_interactions']
  end

  it 'keeps the group attribution of the recorded interactions in the shared cassette' do
    expect(recorded_interaction_hashes.map { |h| h['playback_group'] }).to eq(%w[a a b b])
    expect(recorded_interaction_hashes.map { |h| h['response']['status']['code'] }).to eq([202, 200, 503, 204])
  end

  # Replays, from two interleaved threads, the sequence A, B, B, A, and then
  # lets A make one more request. Returns [results, errors].
  def playback_in_interleaved_threads(repeats)
    order   = [:a, :b, :b, :a, :a]
    lock    = Mutex.new
    cv      = ConditionVariable.new
    step    = 0
    results = { a: [], b: [] }
    errors  = {}

    gate = lambda do |group|
      lock.synchronize do
        cv.wait(lock) until order[step] == group
        step += 1
        cv.broadcast
      end
    end

    play = lambda do |cassette, group|
      # What the request handler does: replay a matching interaction or
      # raise UnhandledHTTPRequestError when none is available.
      response = cassette.http_interactions.response_for(request)
      if response
        results[group] << response.status.code
      else
        errors[group] = VCR::Errors::UnhandledHTTPRequestError.new(request)
      end
    end

    threads = [
      Thread.new do
        VCR.use_cassette('shared', record: :none, playback_group: 'a',
                                   allow_playback_repeats: repeats) do |cassette|
          3.times { gate.call(:a); play.call(cassette, :a) }
        end
      end,
      Thread.new do
        VCR.use_cassette('shared', record: :none, playback_group: 'b',
                                   allow_playback_repeats: repeats) do |cassette|
          2.times { gate.call(:b); play.call(cassette, :b) }
        end
      end
    ]
    threads.each { |t| t.join(30) || t.kill }

    [results, errors]
  end

  context 'when playback repeats are not allowed' do
    it 'plays each group back independently and reports only the exhausted group' do
      results, errors = playback_in_interleaved_threads(false)

      expect(results[:a]).to eq([202, 200])
      expect(results[:b]).to eq([503, 204])
      expect(errors[:b]).to be_nil

      error = errors[:a]
      expect(error).to be_a(VCR::Errors::UnhandledHTTPRequestError)
      # A's interactions are all used up, so the repeats suggestion is offered...
      expect(error.message).to include(':allow_playback_repeats')
      # ...but B's unused interactions are not reported as remaining for A.
      expect(error.message).not_to include('HTTP interactions that have')
    end
  end

  context 'when playback repeats are allowed' do
    it 'replays the used response from the same group' do
      results, errors = playback_in_interleaved_threads(true)

      expect(results[:a]).to eq([202, 200, 200])
      expect(results[:b]).to eq([503, 204])
      expect(errors).to be_empty
    end
  end

  context 'when no playback group is selected' do
    it 'plays interactions back in recorded order, as before' do
      statuses = VCR.use_cassette('shared', record: :none) do |cassette|
        4.times.map { cassette.http_interactions.response_for(request).status.code }
      end

      expect(statuses).to eq([202, 200, 503, 204])
    end
  end

  context 'with a historical cassette recorded without groups' do
    before do
      File.write(File.join(VCR.configuration.cassette_library_dir, 'legacy.yml'), <<~YAML)
        ---
        http_interactions:
        - request:
            method: get
            uri: #{uri}
            body:
              encoding: UTF-8
              string: ''
            headers: {}
          response:
            status:
              code: 200
              message: OK
            headers: {}
            body:
              encoding: UTF-8
              string: legacy
          recorded_at: Tue, 01 Nov 2016 00:00:00 GMT
        recorded_with: VCR 6.0.0
      YAML
    end

    it 'loads and plays back per the original rules' do
      status = VCR.use_cassette('legacy', record: :none) do |cassette|
        cassette.http_interactions.response_for(request).status.code
      end

      expect(status).to eq(200)
    end
  end
end
