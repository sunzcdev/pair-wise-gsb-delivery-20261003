# frozen_string_literal: true

require 'ostruct'

describe 'Association preloading' do
  before { reset_blueprinter_config! }
  after { reset_blueprinter_config! }

  # Plain in-memory objects - no ActiveRecord, no database.
  let(:team_class) do
    Struct.new(:id, :name)
  end

  let(:user_class) do
    Struct.new(:id, :name, :team_id, :active, keyword_init: true) do
      # If the preloaded value is used this method must never be invoked.
      def team
        raise "unexpected per-object team lookup for #{name}"
      end
    end
  end

  let(:team_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
    end
  end

  let(:teams) { { 1 => team_class.new(1, 'A'), 2 => team_class.new(2, 'B') } }

  let(:batch_counter) { [] }

  def build_user_blueprint(team_blueprint:, **association_options)
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      association :team, blueprint: team_blueprint, **association_options
    end
  end

  let(:default_preloader) do
    counter = batch_counter
    lookup = teams
    ->(users) {
      counter << users
      users.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
    }
  end

  let(:blueprint) { build_user_blueprint(team_blueprint:, preload: default_preloader) }

  let(:users) do
    [
      user_class.new(id: 1, name: 'a', team_id: 1, active: true),
      user_class.new(id: 2, name: 'b', team_id: 2, active: true),
      user_class.new(id: 3, name: 'c', team_id: nil, active: true)
    ]
  end

  describe 'batch fetching' do
    it 'invokes the preloader exactly once with the needed objects' do
      blueprint.render_as_hash(users)

      expect(batch_counter.size).to eq(1)
      expect(batch_counter.first.map(&:id)).to eq([1, 2, 3])
    end

    it 'renders association values from the preloaded result' do
      result = blueprint.render_as_hash(users)

      expect(result).to eq([
        { id: 1, name: 'a', team: { id: 1, name: 'A' } },
        { id: 2, name: 'b', team: { id: 2, name: 'B' } },
        { id: 3, name: 'c', team: nil }
      ])
    end

    it 'does not fall back to per-object extraction' do
      # user_class#team raises, so reaching it means batching was bypassed.
      expect { blueprint.render_as_hash(users) }.not_to raise_error
    end

    it 'preloads a repeated object only once while preserving repeated output positions' do
      first = users.first

      result = blueprint.render_as_hash([first, users[1], first])

      expect(batch_counter.size).to eq(1)
      expect(batch_counter.first).to contain_exactly(first, users[1])
      expect(result.length).to eq(3)
      expect(result[0]).to eq(result[2])
      expect(result.map { |hash| hash[:team][:id] }).to eq([1, 2, 1])
    end

    it 'does not invoke the preloader when rendering an empty collection' do
      blueprint.render_as_hash([])

      expect(batch_counter).to be_empty
    end
  end

  describe 'single object rendering' do
    it 'runs the preloader once with an array holding that object' do
      result = blueprint.render_as_hash(users.first)

      expect(batch_counter.size).to eq(1)
      expect(batch_counter.first).to eq([users.first])
      expect(result[:team]).to eq(id: 1, name: 'A')
    end
  end

  describe 'view and exclusion filtering' do
    let(:blueprint) do
      counter = batch_counter
      lookup = teams
      nested_team_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end

      Class.new(Blueprinter::Base) do
        identifier :id
        field :name

        view :with_team do
          association :team,
                      blueprint: nested_team_blueprint,
                      preload: ->(needed) {
                        counter << needed
                        needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                      }
        end

        define_singleton_method(:counter) { counter }
        define_singleton_method(:lookup) { lookup }
      end
    end

    it 'does not preload an association absent from the selected view' do
      blueprint.render_as_hash(users)

      expect(batch_counter).to be_empty
    end

    it 'preloads only when the association is part of the selected view' do
      blueprint.render_as_hash(users, view: :with_team)

      expect(batch_counter.size).to eq(1)
      expect(batch_counter.first.map(&:id)).to eq([1, 2, 3])
    end

    context 'with an excluded association' do
      let(:blueprint) do
        counter = batch_counter
        lookup = teams
        nested = team_blueprint

        Class.new(Blueprinter::Base) do
          identifier :id
          field :name
          association :team,
                      blueprint: nested,
                      preload: ->(needed) {
                        counter << needed
                        needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                      }
          exclude :team
        end
      end

      it 'never invokes the preloader' do
        result = blueprint.render_as_hash(users)

        expect(batch_counter).to be_empty
        expect(result.first).not_to have_key(:team)
      end
    end
  end

  describe ':if / :unless filtering' do
    let(:blueprint) do
      counter = batch_counter
      lookup = teams
      team_id_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
      end

      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        association :team,
                    blueprint: team_id_blueprint,
                    if: ->(_field, user, _options) { user.active },
                    preload: ->(needed) {
                      counter << needed
                      needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                    }
      end
    end

    it 'passes only objects whose conditions pass to the preloader' do
      users.first.active = false

      result = blueprint.render_as_hash(users)

      expect(batch_counter.size).to eq(1)
      expect(batch_counter.first.map(&:id)).to eq([2, 3])
      expect(result.map { |hash| hash[:team] }).to eq([nil, { id: 2 }, nil])
    end

    context 'using :unless' do
      let(:blueprint) do
        counter = batch_counter
        lookup = teams

        Class.new(Blueprinter::Base) do
          identifier :id
          association :team,
                      blueprint: Class.new(Blueprinter::Base) { identifier :id },
                      unless: ->(_field, user, _options) { !user.active },
                      preload: ->(needed) {
                        counter << needed
                        needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                      }
        end
      end

      it 'excludes objects for which :unless is truthy' do
        users[1].active = false

        blueprint.render_as_hash(users)

        expect(batch_counter.first.map(&:id)).to eq([1, 3])
      end
    end
  end

  describe 'preloader result contract' do
    it 'treats a nil entry as an explicitly empty association' do
      preloader = ->(needed) { needed.each_with_object({}) { |user, map| map[user] = nil } }
      blueprint = build_user_blueprint(team_blueprint:, preload: preloader)

      result = blueprint.render_as_hash(users)

      expect(result.map { |hash| hash[:team] }).to eq([nil, nil, nil])
    end

    it 'raises when an object entry is missing instead of querying per-object' do
      partial = users[0..1]
      preloader = ->(needed) {
        batch_counter << needed
        partial.each_with_object({}) { |user, map| map[user] = teams[user.team_id] }
      }
      blueprint = build_user_blueprint(team_blueprint:, preload: preloader)

      expect { blueprint.render_as_hash(users) }.
        to raise_error(Blueprinter::Errors::PreloadResult, /missing a result/)
    end

    it 'raises when the preloader returns something other than a Hash' do
      preloader = ->(_needed) { [] }
      blueprint = build_user_blueprint(team_blueprint:, preload: preloader)

      expect { blueprint.render_as_hash(users) }.
        to raise_error(Blueprinter::Errors::PreloadResult, /must return a Hash/)
    end

    it 'applies default/default_if to preloaded values the same as extracted values' do
      preloader = ->(needed) { needed.each_with_object({}) { |user, map| map[user] = [] } }
      blueprint = build_user_blueprint(
        team_blueprint:,
        preload: preloader,
        default_if: Blueprinter::EMPTY_COLLECTION,
        default: { none: true }
      )

      result = blueprint.render_as_hash(users)

      expect(result.map { |hash| hash[:team] }).to eq([{ none: true }, { none: true }, { none: true }])
    end

    it 'honors exclude_if_nil for preloaded nil values' do
      preloader = ->(needed) { needed.each_with_object({}) { |user, map| map[user] = nil } }
      blueprint = build_user_blueprint(team_blueprint:, preload: preloader, exclude_if_nil: true)

      result = blueprint.render_as_hash(users)

      expect(result.all? { |hash| !hash.key?(:team) }).to be(true)
    end
  end

  describe 'failure isolation between renders' do
    it 'does not carry preloaded data from a failed render into the next render' do
      call_count = 0
      lookup = teams
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :team,
                    blueprint: Class.new(Blueprinter::Base) { identifier :id },
                    preload: ->(needed) {
                      call_count += 1
                      raise 'boom' if call_count == 1

                      needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                    }
      end

      expect { blueprint.render_as_hash(users) }.to raise_error(RuntimeError, 'boom')

      result = blueprint.render_as_hash(users)

      expect(call_count).to eq(2)
      expect(result.map { |hash| hash[:team] }).to eq([{ id: 1 }, { id: 2 }, nil])
    end
  end

  describe 'association aliases' do
    it 'gives each aliased association its own preloader scope' do
      calls = []
      lookup = teams
      nested = team_blueprint

      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :team, name: :home_team, blueprint: nested,
                    preload: ->(needed) {
                      calls << [:home_team, needed]
                      needed.each_with_object({}) { |user, map| map[user] = lookup[user.team_id] }
                    }
        association :team, name: :away_team, blueprint: nested,
                    preload: ->(needed) {
                      calls << [:away_team, needed]
                      needed.each_with_object({}) { |user, map| map[user] = nil }
                    }
      end

      result = blueprint.render_as_hash(users)

      expect(calls.map(&:first)).to contain_exactly(:home_team, :away_team)
      calls.each { |(_, needed)| expect(needed.map(&:id)).to eq([1, 2, 3]) }
      expect(result.first[:home_team]).to eq(id: 1, name: 'A')
      expect(result.first[:away_team]).to be_nil
    end
  end

  describe 'nested associations' do
    it 'runs each preloader once within its own render scope' do
      member_class = Struct.new(:id, :team_id, keyword_init: true)
      nested_batches = []

      member_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :team_id, name: :team_ref
      end

      team_blueprint_with_members = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        association :members,
                    blueprint: member_blueprint,
                    preload: ->(needed_teams) {
                      nested_batches << needed_teams
                      needed_teams.each_with_object({}) do |team, map|
                        map[team] = [member_class.new(id: team.id, team_id: team.id)]
                      end
                    }
      end

      # Each user renders its own collection of teams, so the nested
      # preloader runs once per parent collection scope.
      lookup = { 1 => [teams[1]], 2 => [teams[2]], nil => [] }
      top_batches = []
      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :team,
                    name: :teams,
                    blueprint: team_blueprint_with_members,
                    preload: ->(needed) {
                      top_batches << needed
                      needed.each_with_object({}) { |user, map| map[user] = lookup[user[:team_id]] }
                    }
      end

      result = blueprint.render_as_hash(users)

      expect(top_batches.size).to eq(1)
      expect(top_batches.first.map(&:id)).to eq([1, 2, 3])
      expect(nested_batches.size).to eq(2)
      expect(nested_batches.map { |scope| scope.map(&:id) }).to eq([[1], [2]])
      expect(result[0][:teams].first[:members]).to eq([{ id: 1, team_ref: 1 }])
      expect(result[2][:teams]).to eq([])
    end
  end

  describe 'definition validation' do
    it 'raises when preload is not callable' do
      expect do
        Class.new(Blueprinter::Base) do
          association :team,
                      blueprint: Class.new(Blueprinter::Base) { identifier :id },
                      preload: :not_callable
        end
      end.to raise_error(ArgumentError, /must respond to #call/)
    end
  end

  describe 'local options isolation' do
    it 'does not expose the internal preload context to field blocks or nested options' do
      seen_options = nil
      preloader = ->(needed) { needed.each_with_object({}) { |user, map| map[user] = teams[user.team_id] } }
      nested = Class.new(Blueprinter::Base) do
        identifier :id
      end

      blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field(:snapshot) { |_user, **options| seen_options = options; true }
        association :team,
                    blueprint: nested,
                    options: ->(_user) { { from: :association } },
                    preload: preloader
      end

      blueprint.render_as_hash(users)

      expect(seen_options).not_to have_key(:__blueprinter_preload_context__)
      expect(seen_options[:view]).to eq(:default)
    end
  end
end
