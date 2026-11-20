require 'vcr'
require 'vcr/test_frameworks/rspec'

# Exercises the result-aware cassette ejection performed by the RSpec
# metadata integration when `assert_no_unused_interactions` is in effect
# (i.e. `allow_unused_http_interactions: false`). All three examples share
# the `unused_interactions` cassette fixture, which records 3 interactions.
RSpec.describe VCR::RSpec::Metadata, '.eject_cassette_for' do
  fixture_dir = File.expand_path('../../../../fixtures/rspec_metadata', __FILE__)

  def build_example(exception: nil, pending_message: nil)
    execution_result = instance_double(
      'RSpec::Core::Example::ExecutionResult',
      pending_message: pending_message
    )
    instance_double(
      'RSpec::Core::Example',
      exception: exception,
      metadata: {},
      execution_result: execution_result
    )
  end

  def play_back(uri)
    VCR.current_cassette.http_interactions.response_for(VCR::Request.new(:get, uri))
  end

  before do
    VCR.configure do |c|
      c.cassette_library_dir = fixture_dir
      c.default_cassette_options = { allow_unused_http_interactions: false }
    end
  end

  after do
    VCR.eject_cassette(skip_no_unused_interactions_assertion: true) while VCR.current_cassette
    VCR.configure do |c|
      c.cassette_library_dir = nil
      c.default_cassette_options = { allow_unused_http_interactions: true }
    end
  end

  it 'raises a usage diff with counts for an example that completed normally' do
    VCR.insert_cassette('unused_interactions')
    play_back('http://example.com/one')

    expect {
      VCR::RSpec::Metadata.eject_cassette_for(build_example)
    }.to raise_error(VCR::Errors::UnusedHTTPInteractionError) { |error|
      expect(error.message).to include(
        "There are unused HTTP interactions left in the cassette:"
      )
      expect(error.message).to include(
        "1 of 3 recorded interactions was used; 2 remain unused."
      )
      expect(error.message).to include('http://example.com/two')
      expect(error.message).to include('http://example.com/three')
    }

    expect(VCR.current_cassette).to be_nil
  end

  it 'preserves the first exception of a failed example and warns with context' do
    VCR.insert_cassette('unused_interactions')
    play_back('http://example.com/one')
    original_failure = RuntimeError.new('boom')
    example = build_example(exception: original_failure)

    expect {
      expect {
        VCR::RSpec::Metadata.eject_cassette_for(build_example(exception: original_failure))
      }.not_to raise_error
    }.to output(/already failed with RuntimeError: "boom".*2 remain unused/m).to_stderr

    expect(example.exception).to be(original_failure)
    expect(VCR.current_cassette).to be_nil
  end

  it 'skips the assertion silently for a pending example' do
    VCR.insert_cassette('unused_interactions')

    expect {
      expect {
        VCR::RSpec::Metadata.eject_cassette_for(build_example(pending_message: 'Not yet implemented'))
      }.not_to raise_error
    }.not_to output.to_stderr

    expect(VCR.current_cassette).to be_nil
  end
end
