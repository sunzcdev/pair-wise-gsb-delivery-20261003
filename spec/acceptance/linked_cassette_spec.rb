require 'spec_helper'

# The shared parent cassette is only read from; only the cassette owned by the
# current context may record new interactions. Some requests fall through to
# the parent layer, so linked cassettes get an explicit read/write contract to
# stop downstream contexts from writing new records into the shared fixture
# merely because nothing matched. The child layer can also be made read-only on
# its own, while normal parent playback is unaffected.
RSpec.describe 'linked cassettes across threads' do
  let(:library_dir) { VCR.configuration.cassette_library_dir }
  let(:parent_file) { File.join(library_dir, 'shared_parent.yml') }
  let(:child_file)  { File.join(library_dir, 'child_episodes.yml') }

  def interaction(uri, body)
    request = VCR::Request.new(:get, uri)
    response = VCR::Response.new(VCR::ResponseStatus.new(200), nil, body, '1.1')
    VCR::HTTPInteraction.new(request, response)
  end

  def write_fixture(name, interactions)
    VCR.use_cassette(name, record: :new_episodes) do |cassette|
      interactions.each { |i| cassette.record_http_interaction(i) }
    end
  end

  def uris_in(file)
    YAML.load_file(file)['http_interactions'].map { |h| h['request']['uri'] }
  end

  # Snapshot the two temporary fixtures up front so the assertions compare the
  # exact final contents of both files.
  def snapshot_files
    { parent: File.read(parent_file), child: File.read(child_file) }
  end

  def run_in_linked_thread
    thread = Thread.new do
      # Touching VCR state in a new thread dupes the context and links it back
      # to the parent thread's context, making the parent cassette "linked".
      VCR.turned_on?
      yield
    end
    thread.value
  end

  before do
    write_fixture('shared_parent', [
      interaction('http://example.com/parent-hit', 'parent response')
    ])
    write_fixture('child_episodes', [
      interaction('http://example.com/child-hit', 'child response')
    ])
  end

  describe 'a child context with its own writable cassette' do
    it 'replays hits from both layers, drops missing requests, and records only into the child fixture' do
      before = snapshot_files

      VCR.insert_cassette('shared_parent', record: :new_episodes)

      recorded_interaction = run_in_linked_thread do
        VCR.use_cassette('child_episodes', record: :new_episodes) do
          # 1. Hit in the child cassette itself.
          child_response = VCR.http_interactions.response_for(
            VCR::Request.new(:get, 'http://example.com/child-hit')
          )
          expect(child_response.body).to eq('child response')

          # 2. Miss in the child cassette: falls through to the linked parent
          #    layer and is replayed from the shared fixture.
          parent_response = VCR.http_interactions.response_for(
            VCR::Request.new(:get, 'http://example.com/parent-hit')
          )
          expect(parent_response.body).to eq('parent response')

          # 3. Miss in every layer: the child cassette is recording, so the
          #    request is recordable -- but it must land in the child fixture,
          #    never the shared parent one.
          new_request = VCR::Request.new(:get, 'http://example.com/child-new')
          expect(VCR.http_interactions.has_interaction_matching?(new_request)).to be(false)
          new_interaction = interaction('http://example.com/child-new', 'new response')
          VCR.record_http_interaction(new_interaction)
          new_interaction
        end
      end

      VCR.eject_cassette

      # The shared parent fixture is byte-for-byte unchanged: the falling-through
      # hit and the recordable request did not mutate it.
      expect(File.read(parent_file)).to eq(before[:parent])
      expect(uris_in(parent_file)).to eq(['http://example.com/parent-hit'])

      # The child fixture keeps its own hit, replayed the parent hit without
      # copying it, and gains the newly recorded interaction.
      expect(uris_in(child_file)).to match_array(
        ['http://example.com/child-hit', 'http://example.com/child-new']
      )
      expect(File.read(child_file)).not_to eq(before[:child])
      expect(recorded_interaction).to be_a(VCR::HTTPInteraction)
    end

    it 'never lets a child record straight into the linked parent when it owns no cassette' do
      before = snapshot_files

      VCR.insert_cassette('shared_parent', record: :new_episodes)

      run_in_linked_thread do
        expect(VCR.current_cassette).to be_linked
        expect(VCR.current_cassette).to be_read_only

        # A request the parent fixture cannot satisfy is recordable per the
        # parent's mode, but the linked wrapper must refuse to store it.
        new_request = VCR::Request.new(:get, 'http://example.com/orphan')
        expect(VCR.http_interactions.has_interaction_matching?(new_request)).to be(false)
        VCR.record_http_interaction(interaction('http://example.com/orphan', 'orphan body'))
      end

      VCR.eject_cassette

      expect(File.read(parent_file)).to eq(before[:parent])
      expect(uris_in(parent_file)).to eq(['http://example.com/parent-hit'])
      expect(File.read(child_file)).to eq(before[:child])
    end
  end

  describe 'a child context whose own cassette is read-only' do
    it 'still replays parent and child hits, but records nothing to either file' do
      before = snapshot_files

      VCR.insert_cassette('shared_parent', record: :new_episodes)

      run_in_linked_thread do
        VCR.use_cassette('child_episodes', record: :new_episodes, read_only: true) do |child|
          expect(child).to be_read_only
          expect(child).not_to be_recording

          # Normal playback from both layers is unaffected.
          expect(
            VCR.http_interactions.response_for(
              VCR::Request.new(:get, 'http://example.com/child-hit')
            ).body
          ).to eq('child response')
          expect(
            VCR.http_interactions.response_for(
              VCR::Request.new(:get, 'http://example.com/parent-hit')
            ).body
          ).to eq('parent response')

          # The recordable request is dropped by the read-only child cassette.
          VCR.record_http_interaction(
            interaction('http://example.com/child-new', 'new response')
          )
          expect(child.new_recorded_interactions).to eq([])
        end
      end

      VCR.eject_cassette

      expect(File.read(parent_file)).to eq(before[:parent])
      expect(File.read(child_file)).to eq(before[:child])
      expect(uris_in(child_file)).to eq(['http://example.com/child-hit'])
    end
  end
end
