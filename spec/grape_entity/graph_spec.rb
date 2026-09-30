# frozen_string_literal: true

require 'grape-entity'

describe Grape::Entity::Graph do
  # Plain test models. #bind_pair wires a bidirectional association.
  let(:model_class) do
    Class.new do
      attr_accessor :identity, :secret

      def initialize(identity, secret = nil)
        @identity = identity
        @secret = secret
      end

      def singleton_associations
        @singleton_associations ||= {}
      end

      def method_missing(name, *args, &block)
        singleton_associations.key?(name) ? singleton_associations[name] : super
      end

      def respond_to_missing?(name, include_private = false)
        singleton_associations.key?(name) || super
      end
    end
  end

  let(:model_a) { model_class.new('A', 'a-secret') }
  let(:model_b) { model_class.new('B', 'b-secret') }
  let(:model_c) { model_class.new('C') }

  before do
    model_a.singleton_associations[:b] = model_b
    model_b.singleton_associations[:a] = model_a
    model_c.singleton_associations[:b] = model_b
    model_c.singleton_associations[:friends] = [model_b, model_b]
  end

  let!(:a_entity) do
    stub_const('AEntity', Class.new(Grape::Entity) { identity :identity })
  end

  let!(:b_entity) do
    stub_const('BEntity', Class.new(Grape::Entity) { identity :identity })
  end

  let!(:c_entity) do
    stub_const('CEntity', Class.new(Grape::Entity) { identity :identity })
  end

  before do
    a_entity.expose :identity
    a_entity.expose :secret
    a_entity.expose :b, using: b_entity

    b_entity.expose :identity
    b_entity.expose :secret
    b_entity.expose :a, using: a_entity

    c_entity.expose :identity
    c_entity.expose :b, using: b_entity
    c_entity.expose :friends, using: b_entity
  end

  describe 'cyclic graph A -> B -> A' do
    subject(:document) { a_entity.represent(model_a, graph: true) }

    it 'renders a finite document with the root expanded once' do
      expect(document).to be_a(Hash)
      expect(document[:data]).to eq(
        identity: 'A',
        secret: 'a-secret',
        b: { type: :bs, id: 'B' }
      )
    end

    it 'collects each first-seen entity exactly once' do
      expect(document[:included].size).to eq(1)
      expect(document[:included]).to contain_exactly(
        identity: 'B',
        secret: 'b-secret',
        a: { type: :as, id: 'A' }
      )
    end

    it 'keeps the document finite under JSON serialization' do
      parsed = JSON.parse(document.to_json)
      expect(parsed).to eq(
        'data' => {
          'identity' => 'A',
          'secret' => 'a-secret',
          'b' => { 'type' => 'bs', 'id' => 'B' }
        },
        'included' => [
          {
            'identity' => 'B',
            'secret' => 'b-secret',
            'a' => { 'type' => 'as', 'id' => 'A' }
          }
        ]
      )
    end
  end

  describe 'a third object referencing the same B' do
    subject(:document) do
      c_entity.represent(model_c, graph: true)
    end

    it 'expands C fully and links B in single and collection associations' do
      expect(document[:data]).to eq(
        identity: 'C',
        b: { type: :bs, id: 'B' },
        friends: [{ type: :bs, id: 'B' }, { type: :bs, id: 'B' }]
      )
    end

    it 'expands B only once even through single and collection references' do
      b_payloads = document[:included].select { |payload| payload[:identity] == 'B' }
      expect(b_payloads.size).to eq(1)
      expect(b_payloads.first).to include(a: { type: :as, id: 'A' })
    end

    it 'expands A through the B -> A back-reference as well' do
      identities = document[:included].map { |payload| payload[:identity] }
      expect(identities).to contain_exactly('B', 'A')
    end

    it 'resolves every link against data or included' do
      entries = [document[:data], *document[:included]]
      index = entries.each_with_object({}) do |payload, map|
        type = payload[:identity] == 'A' ? :as : payload[:identity] == 'B' ? :bs : :cs
        map[[type, payload[:identity]]] = payload
      end

      entries.each do |payload|
        payload.each_value do |value|
          links = value.is_a?(Array) ? value : [value]
          links.each do |link|
            next unless link.is_a?(Hash) && link.key?(:type)

            expect(index[[link[:type], link[:id]]]).not_to be_nil
          end
        end
      end
    end
  end

  describe 'identity deduplication across distinct Ruby objects' do
    let(:other_b) do
      model_class.new('B', 'b-secret').tap { |copy| copy.singleton_associations[:a] = model_a }
    end

    it 'expands the same business identity only once' do
      model_c.singleton_associations[:other_friend] = other_b
      c_entity.expose :other_friend, using: b_entity

      document = c_entity.represent(model_c, graph: true)
      b_payloads = document[:included].select { |payload| payload[:identity] == 'B' }
      expect(b_payloads.size).to eq(1)
    end

    it 'compares a same-identity object reached on a cycle while the first one is still expanding' do
      other_a = model_class.new('A', 'a-secret')
      other_a.singleton_associations[:b] = model_b
      model_b.singleton_associations[:a] = other_a

      document = a_entity.represent(model_a, graph: true)
      a_payloads = [document[:data], *document[:included]].select { |payload| payload[:identity] == 'A' }
      expect(a_payloads.size).to eq(1)
    end

    it 'raises when that cyclic same-identity object carries conflicting content' do
      other_a = model_class.new('A', 'CONFLICT')
      other_a.singleton_associations[:b] = model_b
      model_b.singleton_associations[:a] = other_a

      expect { a_entity.represent(model_a, graph: true) }
        .to raise_error(Grape::Entity::Graph::ConflictError, /as "A"/)
    end
  end

  describe 'identity conflicts' do
    let(:conflicting_b) do
      model_class.new('B', 'DIFFERENT').tap { |copy| copy.singleton_associations[:a] = model_a }
    end

    it 'raises instead of silently choosing one representation' do
      model_c.singleton_associations[:other_friend] = conflicting_b
      c_entity.expose :other_friend, using: b_entity

      expect {
        c_entity.represent(model_c, graph: true)
      }.to raise_error(Grape::Entity::Graph::ConflictError, /bs "B"/)
    end
  end

  describe 'field filters and conditions' do
    it 'applies :only filters to the expanded payloads and links' do
      document = a_entity.represent(model_a, graph: true, only: [:identity, b: [:identity]])
      expect(document[:data]).to eq(identity: 'A', b: { type: :bs, id: 'B' })
      expect(document[:included]).to contain_exactly(identity: 'B')
    end

    it 'never reads associations hidden by conditions for deduplication' do
      read = false
      model_b.singleton_associations[:guarded_reader] = -> { read = true; model_a }
      model_b.define_singleton_method(:guarded) { singleton_associations[:guarded_reader].call }
      guarded_a = Class.new(Grape::Entity) { identity :identity; expose :identity }
      b_entity.expose :guarded, using: guarded_a, if: ->(_object, opts) { opts[:show_guarded] }

      document = a_entity.represent(model_a, graph: true)
      expect(read).to eq(false)
      expect(document[:included].first).not_to have_key(:guarded)

      a_entity.represent(model_a, graph: true, show_guarded: true)
      expect(read).to eq(true)
    end
  end

  describe 'collections at the root' do
    it 'renders each root fully and deduplicates shared associations' do
      other_a = model_class.new('A2', 'a2-secret')
      other_a.singleton_associations[:b] = model_b

      document = a_entity.represent([model_a, other_a], graph: true)
      expect(document[:data].map { |payload| payload[:identity] }).to eq(%w[A A2])
      expect(document[:included].map { |payload| payload[:identity] }).to eq(%w[B])
    end
  end

  describe 'missing identity' do
    it 'raises a clear error when a linked entity declares none' do
      anonymous = Class.new(Grape::Entity) { expose :identity }
      holder = Class.new(Grape::Entity) do
        identity :identity
        expose :identity
        expose :friend, using: anonymous
      end
      model_c.singleton_associations[:friend] = model_b

      expect { holder.represent(model_c, graph: true) }
        .to raise_error(Grape::Entity::Graph::MissingIdentityError, /identity/)
    end
  end
end
