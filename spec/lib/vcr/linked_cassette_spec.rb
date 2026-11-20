require 'spec_helper'

RSpec.describe VCR::LinkedCassette do
  let(:fixture_dir) { "#{VCR::SPEC_ROOT}/fixtures/cassette_spec" }
  let(:recorded_request) { VCR::Request.new(:get, 'http://example.com/') }
  let(:new_request) { VCR::Request.new(:get, 'http://example.com/new') }

  let(:new_interaction) do
    response = VCR::Response.new(VCR::ResponseStatus.new(200), nil, 'body', '1.1')
    VCR::HTTPInteraction.new(new_request, response)
  end

  describe 'the read/write contract of a linked (shared parent) cassette' do
    let(:cassette) do
      VCR.configuration.cassette_library_dir = fixture_dir
      VCR::Cassette.new('example', record: :new_episodes)
    end
    subject(:linked) { VCR::LinkedCassette.new(cassette) }

    it 'delegates reads such as #name, #record_mode and #match_requests_on' do
      expect(linked.name).to eq(cassette.name)
      expect(linked.record_mode).to eq(:new_episodes)
      expect(linked.match_requests_on).to eq(cassette.match_requests_on)
    end

    it 'still serves the shared fixture interactions for playback' do
      expect(linked.http_interactions.remaining_unused_interaction_count).to eq(3)
      expect(linked.http_interactions.has_interaction_matching?(recorded_request)).to be(true)
      expect(linked.http_interactions.response_for(recorded_request).body)
        .to match(/You have reached this web page/)
    end

    it 'falls through to the parent list when nothing matches, without recording anything' do
      expect(linked.http_interactions.has_interaction_matching?(new_request)).to be(false)
      linked.record_http_interaction(new_interaction)
      expect(cassette.new_recorded_interactions).to eq([])
      expect(linked.http_interactions.remaining_unused_interaction_count).to eq(3)
    end

    it 'is marked as linked and read-only' do
      expect(linked).to be_linked
      expect(linked).to be_read_only
    end

    it 'never reports itself as recording, even when the underlying cassette does' do
      expect(cassette).to be_recording
      expect(linked).not_to be_recording
    end

    it 'refuses to store new interactions and leaves the underlying cassette untouched' do
      linked.record_http_interaction(new_interaction)
      expect(cassette.new_recorded_interactions).to eq([])
      expect(linked.new_recorded_interactions).to eq([])
    end

    it 'cannot be ejected from the linking context' do
      expect { linked.eject }.to raise_error(VCR::Errors::EjectLinkedCassetteError)
    end

    it 'keeps the shared fixture file untouched when the owning context ejects later' do
      original_content = ::File.read(cassette.file)
      linked.record_http_interaction(new_interaction)
      cassette.eject
      expect(::File.read(cassette.file)).to eq(original_content)
    end
  end

  describe '.list' do
    let(:owned) { VCR::Cassette.new('owned', record: :none) }
    let(:shared) { VCR::Cassette.new('shared', record: :new_episodes) }

    it 'wraps the context-unowned cassettes but leaves owned cassettes unwrapped' do
      list = VCR::LinkedCassette.list([owned], [shared])

      expect(list.to_a.map(&:class)).to eq([VCR::LinkedCassette, VCR::Cassette])
      expect(list.first).to be_read_only
      expect(list.last).not_to be_read_only
    end

    it 'does not double-wrap an already linked cassette' do
      already_linked = VCR::LinkedCassette.new(shared)
      list = VCR::LinkedCassette.list([], [already_linked])

      expect(list.to_a).to eq([already_linked])
    end

    it 'prefers the context-owned cassette as #last' do
      list = VCR::LinkedCassette.list([owned], [shared])
      expect(list.last).to be(owned)
    end

    it 'falls back to the linked cassette as #last when the context owns none' do
      list = VCR::LinkedCassette.list([], [shared])
      expect(list.last).to be_a(VCR::LinkedCassette)
      expect(list.last.name).to eq('shared')
    end
  end
end
