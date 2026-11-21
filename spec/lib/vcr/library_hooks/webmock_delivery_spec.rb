require 'rspec'
require 'net/http'
require 'vcr'
require 'vcr/library_hooks/webmock'

# Self-contained coverage for playback settlement at the WebMock delivery
# boundary: WebMock asks VCR for a stubbed response and then builds the
# library response that the test receives. That second step can still fail, and
# in that case the matched interaction must remain replayable.
#
# Deliberately does not use spec_helper, which needs every supported HTTP
# library; this only needs WebMock and net/http.
RSpec.describe "VCR WebMock playback delivery" do
  def reset_vcr!
    VCR.instance_variables.each { |ivar| VCR.instance_variable_set(ivar, nil) }
    VCR.send(:initialize_ivars)
    VCR.turn_on!
  end

  let(:request_url) { "http://reserved.example.com/only" }

  let(:interaction) do
    VCR::HTTPInteraction.new(
      VCR::Request.new(:get, request_url),
      VCR::Response.new(VCR::ResponseStatus.new(200, 'OK'), {}, 'reserved body', '1.1')
    )
  end

  let(:list) { VCR::Cassette::HTTPInteractionList.new([interaction], [:method, :uri]) }

  before do
    reset_vcr!
    allow(VCR).to receive(:http_interactions).and_return(list)
  end

  after do
    ::WebMock::CallbackRegistry.callbacks.delete_if { |callback| callback[:vcr_delivery_spec] }
  end

  def get_body
    Net::HTTP.get(URI(request_url))
  end

  it 'consumes the interaction once the response reached the test' do
    expect(get_body).to eq('reserved body')
    expect(list.remaining_unused_interaction_count).to eq(0)
  end

  it 'only consumes the interaction after WebMock reached its delivery boundary' do
    observed = nil
    ::WebMock::CallbackRegistry.add_callback(
      { vcr_delivery_spec: true },
      proc { |_request, _response| observed = list.remaining_unused_interaction_count }
    )

    expect(get_body).to eq('reserved body')
    expect(observed).to eq(1)
    expect(list.remaining_unused_interaction_count).to eq(0)
  end

  it 'returns the interaction when delivery fails, so the next request replays it' do
    failures = 0
    ::WebMock::CallbackRegistry.add_callback(
      { vcr_delivery_spec: true },
      proc do |_request, _response|
        failures += 1
        raise 'adapter failed before delivering the response' if failures == 1
      end
    )

    expect { get_body }.to raise_error(/failed before delivering/)
    expect(list.remaining_unused_interaction_count).to eq(1)

    expect(get_body).to eq('reserved body')
    expect(list.remaining_unused_interaction_count).to eq(0)
  end

  it 'gives one blocked request exclusive ownership until it fails, then lets the next request replay' do
    inside = Queue.new
    release = Queue.new
    blocking = true

    ::WebMock::CallbackRegistry.add_callback(
      { vcr_delivery_spec: true },
      proc do |_request, _response|
        if blocking
          blocking = false
          inside << true
          release.pop
          raise 'adapter aborted before delivery'
        end
      end
    )

    holder = Thread.start do
      begin
        get_body
      rescue RuntimeError => e
        e.message
      end
    end

    inside.pop
    concurrent = begin
      get_body
    rescue VCR::Errors::UnhandledHTTPRequestError
      :unhandled
    end
    release << true

    expect(concurrent).to eq(:unhandled)
    expect(holder.value).to include('aborted before delivery')
    expect(list.remaining_unused_interaction_count).to eq(1)

    expect(get_body).to eq('reserved body')
    expect(list.remaining_unused_interaction_count).to eq(0)
  end
end
