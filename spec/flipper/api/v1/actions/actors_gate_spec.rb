RSpec.describe Flipper::Api::V1::Actions::ActorsGate do
  let(:app) { build_api(flipper) }
  let(:actor) { Flipper::Actor.new('1') }

  describe 'enable' do
    before do
      flipper[:my_feature].disable_actor(actor)
      post '/features/my_feature/actors', flipper_id: actor.flipper_id
    end

    it 'enables feature for actor' do
      expect(last_response.status).to eq(200)
      expect(flipper[:my_feature].enabled?(actor)).to be_truthy
      expect(flipper[:my_feature].enabled_gate_names).to eq([:actor])
    end

    it 'returns decorated feature with actor enabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to eq(['1'])
    end
  end

  describe 'disable' do
    let(:actor) { Flipper::Actor.new('1') }

    before do
      flipper[:my_feature].enable_actor(actor)
      delete '/features/my_feature/actors', flipper_id: actor.flipper_id
    end

    it 'disables feature' do
      expect(last_response.status).to eq(200)
      expect(flipper[:my_feature].enabled?(actor)).to be_falsy
      expect(flipper[:my_feature].enabled_gate_names).to be_empty
    end

    it 'returns decorated feature with boolean gate disabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to be_empty
    end
  end

  describe 'enable feature with slash in name' do
    before do
      flipper["my/feature"].disable_actor(actor)
      post '/features/my/feature/actors', flipper_id: actor.flipper_id
    end

    it 'enables feature for actor' do
      expect(last_response.status).to eq(200)
      expect(flipper["my/feature"].enabled?(actor)).to be_truthy
      expect(flipper["my/feature"].enabled_gate_names).to eq([:actor])
    end

    it 'returns decorated feature with actor enabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to eq(['1'])
    end
  end

  describe 'enable feature with space in name' do
    before do
      flipper["sp ace"].disable_actor(actor)
      post '/features/sp%20ace/actors', flipper_id: actor.flipper_id
    end

    it 'enables feature for actor' do
      expect(last_response.status).to eq(200)
      expect(flipper["sp ace"].enabled?(actor)).to be_truthy
      expect(flipper["sp ace"].enabled_gate_names).to eq([:actor])
    end

    it 'returns decorated feature with actor enabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to eq(['1'])
    end
  end

  describe 'enable missing flipper_id parameter' do
    before do
      flipper[:my_feature].enable
      post '/features/my_feature/actors'
    end

    it 'returns correct error response' do
      expect(last_response.status).to eq(422)
      expect(json_response).to eq(api_flipper_id_is_missing_response)
    end
  end

  describe 'disable missing flipper_id parameter' do
    before do
      flipper[:my_feature].enable
      delete '/features/my_feature/actors'
    end

    it 'returns correct error response' do
      expect(last_response.status).to eq(422)
      expect(json_response).to eq(api_flipper_id_is_missing_response)
    end
  end

  describe 'enable nil flipper_id parameter' do
    before do
      flipper[:my_feature].enable
      post '/features/my_feature/actors', flipper_id: nil
    end

    it 'returns correct error response' do
      expect(last_response.status).to eq(422)
      expect(json_response).to eq(api_flipper_id_is_missing_response)
    end
  end

  describe 'disable nil flipper_id parameter' do
    before do
      flipper[:my_feature].enable
      delete '/features/my_feature/actors', flipper_id: nil
    end

    it 'returns correct error response' do
      expect(last_response.status).to eq(422)
      expect(json_response).to eq(api_flipper_id_is_missing_response)
    end
  end

  describe 'enable missing feature' do
    before do
      post '/features/my_feature/actors', flipper_id: actor.flipper_id
    end

    it 'enables feature for actor' do
      expect(last_response.status).to eq(200)
      expect(flipper[:my_feature].enabled?(actor)).to be_truthy
      expect(flipper[:my_feature].enabled_gate_names).to eq([:actor])
    end

    it 'returns decorated feature with actor enabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to eq(['1'])
    end
  end

  describe 'disable missing feature' do
    before do
      delete '/features/my_feature/actors', flipper_id: actor.flipper_id
    end

    it 'disables feature' do
      expect(last_response.status).to eq(200)
      expect(flipper[:my_feature].enabled?(actor)).to be_falsy
      expect(flipper[:my_feature].enabled_gate_names).to be_empty
    end

    it 'returns decorated feature with boolean gate disabled' do
      gate = json_response['gates'].find { |gate| gate['key'] == 'actors' }
      expect(gate['value']).to be_empty
    end
  end

  describe 'replace' do
    let(:old_actor) { Flipper::Actor.new('old') }
    let(:new_actor) { Flipper::Actor.new('new') }

    before do
      flipper[:my_feature].enable_actor(old_actor)
    end

    it 'swaps the actor in a single change' do
      put '/features/my_feature/actors',
          flipper_id: new_actor.flipper_id,
          replace_flipper_id: old_actor.flipper_id

      expect(last_response.status).to eq(200)
      expect(flipper[:my_feature].actors_value).to eq(Set['new'])
      expect(flipper[:my_feature].enabled?(new_actor)).to be(true)
      expect(flipper[:my_feature].enabled?(old_actor)).to be(false)
    end

    it 'succeeds at the actor limit because the set does not grow' do
      limited = Flipper.new(Flipper::Adapters::ActorLimit.new(build_memory_adapter, 1))
      limited[:my_feature].enable_actor(old_actor)

      put '/features/my_feature/actors',
          { flipper_id: new_actor.flipper_id, replace_flipper_id: old_actor.flipper_id },
          'flipper' => limited

      expect(last_response.status).to eq(200)
      expect(limited[:my_feature].actors_value).to eq(Set['new'])
    end

    it 'returns 409 and changes nothing when the observed members are stale' do
      put '/features/my_feature/actors',
          flipper_id: new_actor.flipper_id,
          replace_flipper_id: old_actor.flipper_id,
          expected: %w[old ghost]

      expect(last_response.status).to eq(409)
      expect(json_response['code']).to eq(10)
      expect(flipper[:my_feature].actors_value).to eq(Set['old'])
    end

    it 'requires replace_flipper_id' do
      put '/features/my_feature/actors', flipper_id: new_actor.flipper_id

      expect(last_response.status).to eq(422)
      expect(json_response['code']).to eq(12)
    end
  end
end
