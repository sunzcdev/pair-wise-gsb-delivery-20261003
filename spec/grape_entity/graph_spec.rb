# frozen_string_literal: true

describe Grape::Entity::Graph do
  module GraphSpec
    class NodeA
      attr_accessor :id, :name, :b, :secret

      def initialize(id, name)
        @id = id
        @name = name
      end
    end

    class NodeB
      attr_accessor :id, :label, :a

      def initialize(id, label)
        @id = id
        @label = label
      end
    end

    class NodeC
      attr_accessor :id, :b

      def initialize(id)
        @id = id
      end
    end

    class ExplodingNode
      attr_reader :id

      def initialize(id)
        @id = id
      end

      def hidden_association
        raise 'hidden association was read'
      end
    end

    class AEntity < Grape::Entity
      identity :id
      expose :id
      expose :name
      expose :b, using: 'GraphSpec::BEntity'
    end

    class BEntity < Grape::Entity
      identity :id
      expose :id
      expose :label
      expose :a, using: 'GraphSpec::AEntity'
    end

    class CEntity < Grape::Entity
      identity :id
      expose :id
      expose :b, using: 'GraphSpec::BEntity'
    end

    class ConditionalEntity < Grape::Entity
      identity :id
      expose :id
      expose :name, if: ->(_object, options) { options[:verbose] }
      expose :b, using: 'GraphSpec::BEntity', unless: ->(_object, options) { options[:skip_b] }
    end

    class HiddenAssociationEntity < Grape::Entity
      identity :id
      expose :id
      expose :hidden_association, using: 'GraphSpec::BEntity', if: ->(_object, _options) { false }
    end

    class AnonymousAssociationEntity < Grape::Entity
      expose :id
      expose :b, using: 'GraphSpec::BEntity'
    end
  end

  def cyclic_graph
    a = GraphSpec::NodeA.new(1, 'first')
    b = GraphSpec::NodeB.new(2, 'second')
    a.b = b
    b.a = a
    [a, b]
  end

  describe '.represent with graph: true' do
    it 'terminates on cyclic object graphs' do
      a, = cyclic_graph

      result = nil
      expect { result = GraphSpec::AEntity.represent(a, graph: true) }.not_to raise_error
      expect(result[:data]).to eq(_ref: 'GraphSpec::AEntity:1')
    end

    it 'collects each entity once in the included collection' do
      a, = cyclic_graph

      result = GraphSpec::AEntity.represent(a, graph: true)

      expect(result[:included].keys).to contain_exactly('GraphSpec::AEntity:1', 'GraphSpec::BEntity:2')
      expect(result[:included]['GraphSpec::AEntity:1']).to eq(id: 1, name: 'first', b: { _ref: 'GraphSpec::BEntity:2' })
      expect(result[:included]['GraphSpec::BEntity:2']).to eq(id: 2, label: 'second', a: { _ref: 'GraphSpec::AEntity:1' })
    end

    it 'resolves every link to an entity in the included collection' do
      a, = cyclic_graph
      c = GraphSpec::NodeC.new(3)
      c.b = a.b

      result = GraphSpec::CEntity.represent(c, graph: true)

      links = []
      collect_links = lambda do |node|
        case node
        when Hash
          links << node[:_ref] if node.key?(:_ref)
          node.each_value { |value| collect_links.call(value) }
        when Array
          node.each { |value| collect_links.call(value) }
        end
      end
      collect_links.call(result[:data])
      collect_links.call(result[:included])

      expect(links).not_to be_empty
      expect(links - result[:included].keys).to be_empty
    end

    it 'expands the same business identity only once across different Ruby objects' do
      a, b = cyclic_graph
      b_copy = GraphSpec::NodeB.new(2, 'second')
      b_copy.a = a
      c = GraphSpec::NodeC.new(3)
      c.b = b_copy

      result = GraphSpec::CEntity.represent(c, graph: true)

      expect(result[:included].keys).to contain_exactly(
        'GraphSpec::CEntity:3', 'GraphSpec::BEntity:2', 'GraphSpec::AEntity:1'
      )
      expect(result[:included]['GraphSpec::CEntity:3'][:b]).to eq(_ref: 'GraphSpec::BEntity:2')
      expect(b).not_to be(b_copy)
    end

    it 'raises a ConflictError when the same identity has conflicting content' do
      _, b = cyclic_graph
      conflicting_b = GraphSpec::NodeB.new(2, 'tampered')
      conflicting_b.a = b.a

      expect do
        GraphSpec::BEntity.represent([b, conflicting_b], graph: true)
      end.to raise_error(Grape::Entity::Graph::ConflictError, /GraphSpec::BEntity:2/)
    end

    it 'represents root collections as an array of links' do
      a, = cyclic_graph
      other = GraphSpec::NodeA.new(9, 'other')
      other.b = a.b

      result = GraphSpec::AEntity.represent([a, other], graph: true)

      expect(result[:data]).to eq([{ _ref: 'GraphSpec::AEntity:1' }, { _ref: 'GraphSpec::AEntity:9' }])
      expect(result[:included].keys).to contain_exactly(
        'GraphSpec::AEntity:1', 'GraphSpec::AEntity:9', 'GraphSpec::BEntity:2'
      )
    end

    it 'applies field filtering to the included representations' do
      a, = cyclic_graph

      result = GraphSpec::AEntity.represent(a, graph: true, only: [:id, { b: [:id] }])

      expect(result[:included]['GraphSpec::AEntity:1']).to eq(id: 1, b: { _ref: 'GraphSpec::BEntity:2' })
      expect(result[:included]['GraphSpec::BEntity:2']).to eq(id: 2)
    end

    it 'applies conditions to the included representations' do
      a, = cyclic_graph

      result = GraphSpec::ConditionalEntity.represent(a, graph: true, verbose: false, skip_b: true)

      expect(result[:included]['GraphSpec::ConditionalEntity:1']).to eq(id: 1)
      expect(result[:included].keys).to eq(['GraphSpec::ConditionalEntity:1'])
    end

    it 'does not read associations hidden by conditions' do
      object = GraphSpec::ExplodingNode.new(7)

      result = nil
      expect { result = GraphSpec::HiddenAssociationEntity.represent(object, graph: true) }.not_to raise_error
      expect(result[:included]).to eq('GraphSpec::HiddenAssociationEntity:7' => { id: 7 })
    end

    it 'does not read associations filtered out by :except' do
      object = GraphSpec::ExplodingNode.new(7)

      result = nil
      expect do
        result = GraphSpec::HiddenAssociationEntity.represent(object, graph: true, except: [:hidden_association])
      end.not_to raise_error
      expect(result[:included]).to eq('GraphSpec::HiddenAssociationEntity:7' => { id: 7 })
    end

    it 'links associations of entities without a declared identity' do
      _, b = cyclic_graph
      wrapper = double('wrapper', id: 5, b: b)

      result = GraphSpec::AnonymousAssociationEntity.represent(wrapper, graph: true)

      expect(result[:data]).to eq(id: 5, b: { _ref: 'GraphSpec::BEntity:2' })
      expect(result[:included].keys).to contain_exactly('GraphSpec::BEntity:2', 'GraphSpec::AEntity:1')
    end

    it 'leaves serializable_hash untouched' do
      a, b = cyclic_graph
      b.a = nil

      hash = GraphSpec::AEntity.new(a).serializable_hash

      expect(hash[:id]).to eq(1)
      expect(hash[:b][:id]).to eq(2)
      expect(hash[:b][:a]).to be_nil
    end
  end
end
