require 'yaml'
require 'tmpdir'
require 'fileutils'
require 'vcr'
require 'vcr/cassette'
require 'vcr/errors'

module VCR
  class Cassette
    ::RSpec.describe "Composable request matcher declarations" do
      FIXTURE = File.expand_path('../../../fixtures/composable_match_requests_on.yml', File.dirname(__FILE__))

      def request(method, body = '')
        VCR::Request.new(method, 'http://example.com/legacy/search', body, {})
      end

      # Plays the requests through a fresh cassette and returns, for each of
      # them, the method, the body and the response body that was consumed.
      def play(matchers, requests, cassette_name = 'legacy')
        cassette = Cassette.new(cassette_name, record: :none, match_requests_on: matchers)

        requests.map do |req|
          response = cassette.http_interactions.response_for(req)
          [req.method, req.body, response && response.body]
        end
      end

      before do
        @cassette_dir = Dir.mktmpdir
        FileUtils.cp(FIXTURE, File.join(@cassette_dir, 'legacy.yml'))
        VCR.configuration.cassette_library_dir = @cassette_dir
      end

      after do
        FileUtils.rm_rf(@cassette_dir)
      end

      let(:post_only)  { lambda { |r1, r2| r1.method == :post && r2.method == :post } }
      let(:other_only) { lambda { |r1, r2| r1.method != :post && r2.method != :post } }

      # The legacy API only needs the body compared for POST requests.
      let(:composed_matchers) do
        [
          VCR.request_matchers.any_of(
            VCR.request_matchers.all_of(post_only, :uri, :body),
            VCR.request_matchers.all_of(other_only, :uri)
          )
        ]
      end

      it 'consumes the same-path record whose body matched' do
        expect(play(composed_matchers, [request(:post, 'q=beta')])).to eq([
          [:post, 'q=beta', 'beta body record']
        ])
      end

      it 'consumes the URL-only record for a method that does not need the body' do
        expect(play(composed_matchers, [request(:get)])).to eq([
          [:get, '', 'get url record']
        ])
      end

      it 'does not consume any same-path record when the body branch fails' do
        expect(play(composed_matchers, [request(:post, 'q=gamma')])).to eq([
          [:post, 'q=gamma', nil]
        ])
      end

      it 'still consumes the first same-path record under the legacy array matchers' do
        expect(play([:method, :uri], [request(:post, 'q=beta')])).to eq([
          [:post, 'q=beta', 'alpha body record']
        ])
      end

      it 'leaves the unneeded interactions unused' do
        cassette = Cassette.new('legacy', record: :none, match_requests_on: composed_matchers)
        cassette.http_interactions.response_for(request(:post, 'q=beta'))

        expect(cassette.http_interactions.remaining_unused_interaction_count).to eq(2)
      end

      it 'replaces only the same-path record that the composition matches when re-recording' do
        cassette = Cassette.new('legacy', record: :all, match_requests_on: composed_matchers)
        cassette.record_http_interaction(
          VCR::HTTPInteraction.new(request(:post, 'q=beta'), VCR::Response.new(VCR::ResponseStatus.new(200), {}, 're-recorded beta'))
        )
        cassette.eject

        saved = YAML.load_file(File.join(@cassette_dir, 'legacy.yml'))['http_interactions'].map { |h| h['response']['body']['string'] }
        expect(saved).to eq(['alpha body record', 'get url record', 're-recorded beta'])
      end

      it 'describes the request body in the unhandled request error, as the composition includes it' do
        VCR.configuration.default_cassette_options[:match_requests_on] = composed_matchers

        message = VCR::Errors::UnhandledHTTPRequestError.new(request(:post, 'q=gamma')).message
        expect(message).to include('Body: q=gamma')
      end

      it 'does not describe the request body when no branch includes it' do
        VCR.configuration.default_cassette_options[:match_requests_on] = [
          VCR.request_matchers.any_of(:method, :uri)
        ]

        message = VCR::Errors::UnhandledHTTPRequestError.new(request(:post, 'q=gamma')).message
        expect(message).not_to include('Body: q=gamma')
      end
    end
  end
end
