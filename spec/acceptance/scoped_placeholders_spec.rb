require 'spec_helper'

# A public, non-secret test value. It appears in three places of the same
# interaction: a URI query parameter, a request header and the request body.
RSpec.describe 'scoped cassette placeholders', with_monkey_patches: :webmock do
  let(:secret) { 'hunter2' }
  let(:placeholder) { '<TOKEN>' }
  let(:port) { VCR::SinatraApp.port }

  def get_with_token
    uri = URI("http://localhost:#{port}/search?api_key=#{secret}&other=#{secret}")
    Net::HTTP.start(uri.host, uri.port) do |http|
      request = Net::HTTP::Get.new(uri.request_uri)
      request['X-Api-Key'] = "token=#{secret}"
      request['X-Other'] = secret
      request.body = "the body repeats #{secret} once more"
      http.request(request)
    end
  end

  def cassette_contents(name)
    VCR.cassette_persisters[:file_system]["#{name}.yml"].to_s
  end

  def request_section(name)
    yaml = YAML.safe_load(cassette_contents(name), permitted_classes: [Time, Symbol])
    yaml.fetch('http_interactions').first.fetch('request')
  end

  describe 'a placeholder scoped to the URI query string' do
    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(placeholder, scope: :query) { secret }
      end
    end

    it 'replaces the value only in the query on disk and restores it for playback' do
      recorded_response = played_back_response = nil

      VCR.use_cassette('scoped_query', record: :new_episodes) do
        recorded_response = get_with_token
      end

      on_disk = cassette_contents('scoped_query')
      request = request_section('scoped_query')

      # On disk: the registered query component holds the placeholder (both params)...
      expect(request.fetch('uri')).to include("api_key=#{placeholder}", "other=#{placeholder}")
      # ...and the same value elsewhere in the interaction is left untouched.
      expect(request.fetch('headers')['X-Api-Key']).to eq(["token=#{secret}"])
      expect(request.fetch('headers')['X-Other']).to eq([secret])
      expect(request.fetch('body')).to be_a(Hash)
      expect(request.fetch('body').fetch('string')).to include(secret)
      expect(on_disk).not_to include("api_key=#{secret}")

      VCR.use_cassette('scoped_query', record: :none) do
        played_back_response = get_with_token
      end

      # Playback restores only the registered query range; the response still matches.
      expect(played_back_response.body).to eq(recorded_response.body)
      # The bytes persisted on disk never change during playback.
      expect(cassette_contents('scoped_query')).to eq(on_disk)
    end

    it 'does not restore a placeholder that happens to appear in an unregistered field' do
      VCR.use_cassette('scoped_query_isolation', record: :new_episodes) do
        get_with_token
      end

      # Another placeholder occurrence lands in a field outside the registered
      # scope (the body). Playback substitution must not touch it.
      on_disk = cassette_contents('scoped_query_isolation')
      tampered = on_disk.gsub("the body repeats #{secret} once more",
                               "the body repeats #{placeholder} once more")
      VCR.cassette_persisters[:file_system]['scoped_query_isolation.yml'] = tampered

      VCR.use_cassette('scoped_query_isolation', record: :none) do
        get_with_token
      end

      loaded = YAML.safe_load(tampered, permitted_classes: [Time, Symbol])
      request_body = loaded.fetch('http_interactions').first.fetch('request').fetch('body').fetch('string')
      expect(request_body).to include(placeholder)
      expect(request_body).not_to include(secret)
    end
  end

  describe 'a placeholder scoped to a specific request header' do
    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(placeholder,
                                      scope: { request: { headers: 'X-Api-Key' } }) { secret }
      end
    end

    it 'replaces the value only in that header on disk and restores it for playback' do
      recorded_response = played_back_response = nil

      VCR.use_cassette('scoped_header', record: :new_episodes) do
        recorded_response = get_with_token
      end

      on_disk = cassette_contents('scoped_header')
      request = request_section('scoped_header')

      expect(request.fetch('headers')['X-Api-Key']).to eq(["token=#{placeholder}"])
      # The identical value in another header, the query and the body is untouched.
      expect(request.fetch('headers')['X-Other']).to eq([secret])
      expect(request.fetch('uri')).to include("api_key=#{secret}")
      expect(request.fetch('body').fetch('string')).to include(secret)

      VCR.use_cassette('scoped_header', record: :none) do
        played_back_response = get_with_token
      end

      expect(played_back_response.body).to eq(recorded_response.body)
      expect(cassette_contents('scoped_header')).to eq(on_disk)
    end
  end

  describe 'a placeholder scoped to the request body' do
    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(placeholder, scope: :request_body) { secret }
      end
    end

    it 'replaces the value only in the body on disk and restores it for playback' do
      recorded_response = played_back_response = nil

      VCR.use_cassette('scoped_body', record: :new_episodes) do
        recorded_response = get_with_token
      end

      on_disk = cassette_contents('scoped_body')
      request = request_section('scoped_body')

      expect(request.fetch('body').fetch('string')).to include("the body repeats #{placeholder} once more")
      expect(request.fetch('uri')).to include("api_key=#{secret}")
      expect(request.fetch('headers')['X-Api-Key']).to eq(["token=#{secret}"])

      VCR.use_cassette('scoped_body', record: :none) do
        played_back_response = get_with_token
      end

      expect(played_back_response.body).to eq(recorded_response.body)
      expect(cassette_contents('scoped_body')).to eq(on_disk)
    end
  end

  describe 'the existing unscoped placeholder behavior' do
    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(placeholder) { secret }
      end
    end

    it 'still replaces the value in every part of the interaction' do
      VCR.use_cassette('unscoped', record: :new_episodes) do
        get_with_token
      end

      on_disk = cassette_contents('unscoped')
      request = request_section('unscoped')

      expect(request.fetch('uri')).to include("api_key=#{placeholder}")
      expect(request.fetch('headers')['X-Api-Key']).to eq(["token=#{placeholder}"])
      expect(request.fetch('body').fetch('string')).to include("the body repeats #{placeholder} once more")
      expect(on_disk).not_to include(secret)
    end
  end

  describe 'a body placeholder combined with preserve_exact_body_bytes' do
    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(placeholder, scope: :request_body) { secret }
      end
    end

    it 'still base64-encodes the filtered body so the exact bytes round-trip' do
      VCR.use_cassette('preserved_body', record: :new_episodes, tag: :preserve_exact_body_bytes) do
        get_with_token
      end

      request = request_section('preserved_body')
      body = request.fetch('body')

      # The raw-bytes convention wins: the body is stored base64-encoded...
      expect(body).to have_key('base64_string')
      expect(body).not_to have_key('string')
      # ...and the placeholder substitution was applied before encoding.
      decoded = Base64.decode64(body.fetch('base64_string'))
      expect(decoded).to include("the body repeats #{placeholder} once more")
      expect(decoded).not_to include(secret)
      # The query and headers remain plain, human-readable YAML.
      expect(request.fetch('uri')).to include("api_key=#{secret}")

      # Playback restores the original bytes through the same scoped substitution.
      played_back_response = nil
      VCR.use_cassette('preserved_body', record: :none, tag: :preserve_exact_body_bytes) do
        played_back_response = get_with_token
      end
      expect(played_back_response).to be_a(Net::HTTPOK)
    end
  end

  describe 'a response body placeholder that changes the body length' do
    let(:long_placeholder) { '<A-MUCH-LONGER-PLACEHOLDER>' }

    def request_echoing_secret
      # SinatraApp's /search responds with "query: <q>", so the secret ends up
      # in the response body with a different length once placeholders expand.
      uri = URI("http://localhost:#{port}/search?q=#{secret}")
      Net::HTTP.get_response(uri)
    end

    before do
      VCR.configure do |c|
        c.define_cassette_placeholder(long_placeholder, scope: :response_body) { secret }
      end
    end

    it 'keeps Content-Length consistent with the update_content_length_header option' do
      VCR.use_cassette('content_length', record: :new_episodes,
                                          update_content_length_header: true) do
        request_echoing_secret
      end

      yaml = YAML.safe_load(cassette_contents('content_length'), permitted_classes: [Time, Symbol])
      response = yaml.fetch('http_interactions').first.fetch('response')
      stored_body = response.fetch('body').fetch('string')
      expect(stored_body).to include(long_placeholder)

      # On playback the built-in hook recomputes Content-Length from the
      # restored (original length) body.
      played_back_response = nil
      VCR.use_cassette('content_length', record: :none,
                                          update_content_length_header: true) do
        played_back_response = request_echoing_secret
      end

      expect(played_back_response.body).to eq("query: #{secret}")
      expect(played_back_response['Content-Length']).to eq(played_back_response.body.bytesize.to_s)
    end
  end
end
