# frozen_string_literal: true

describe 'Polymorphic exposures' do
  let(:article_class) do
    Class.new do
      attr_accessor :title

      def initialize(title)
        @title = title
      end
    end
  end

  let(:video_class) do
    Class.new do
      attr_accessor :url

      def initialize(url)
        @url = url
      end
    end
  end

  let(:article_entity) do
    Class.new(Grape::Entity) do
      expose :title
      expose(:kind) { |_| 'article' }
    end
  end

  let(:video_entity) do
    Class.new(Grape::Entity) do
      expose :url
      expose(:kind) { |_| 'video' }
    end
  end

  let(:fallback_entity) do
    Class.new(Grape::Entity) do
      expose(:kind) { |_| 'fallback' }
    end
  end

  def build_entity(mapping)
    Class.new(Grape::Entity) do
      expose :items, polymorphic: mapping
    end
  end

  def feed(*items)
    Struct.new(:items).new(items)
  end

  context 'with a mixed collection' do
    it 'dispatches each element individually by its actual type' do
      entity = build_entity(article_class => article_entity, video_class => video_entity)
      model = feed(article_class.new('Hello'), video_class.new('http://example.com/v'), article_class.new('World'))

      expect(entity.represent(model, serializable: true)).to eq(
        items: [
          { title: 'Hello', kind: 'article' },
          { url: 'http://example.com/v', kind: 'video' },
          { title: 'World', kind: 'article' }
        ]
      )
    end

    it 'keeps nil elements as nil' do
      entity = build_entity(article_class => article_entity, default: fallback_entity)
      model = feed(article_class.new('Hello'), nil)

      expect(entity.represent(model, serializable: true)).to eq(
        items: [{ title: 'Hello', kind: 'article' }, nil]
      )
    end
  end

  context 'with a single object' do
    it 'dispatches on the actual type' do
      mapping = { article_class => article_entity, video_class => video_entity }
      entity = Class.new(Grape::Entity) do
        expose :item, polymorphic: mapping
      end
      model = Struct.new(:item).new(video_class.new('http://example.com/v'))

      expect(entity.represent(model, serializable: true)).to eq(
        item: { url: 'http://example.com/v', kind: 'video' }
      )
    end

    it 'represents nil as nil' do
      mapping = { article_class => article_entity }
      entity = Class.new(Grape::Entity) do
        expose :item, polymorphic: mapping
      end
      model = Struct.new(:item).new(nil)

      expect(entity.represent(model, serializable: true)).to eq(item: nil)
    end
  end

  context 'with class inheritance' do
    let(:featured_article_class) { Class.new(article_class) }

    it 'prefers the most specific class rule' do
      featured_entity = Class.new(Grape::Entity) do
        expose :title
        expose(:kind) { |_| 'featured' }
      end

      entity = build_entity(article_class => article_entity, featured_article_class => featured_entity)
      model = feed(featured_article_class.new('Hello'), article_class.new('World'))

      expect(entity.represent(model, serializable: true)).to eq(
        items: [
          { title: 'Hello', kind: 'featured' },
          { title: 'World', kind: 'article' }
        ]
      )
    end

    it 'falls back to a superclass rule when no specific rule matches' do
      entity = build_entity(article_class => article_entity)
      model = feed(featured_article_class.new('Hello'))

      expect(entity.represent(model, serializable: true)).to eq(
        items: [{ title: 'Hello', kind: 'article' }]
      )
    end
  end

  context 'with module rules' do
    let(:taggable) { Module.new }
    let(:searchable) { Module.new }
    let(:featured) { Module.new.tap { |mod| mod.include(taggable) } }

    let(:taggable_entity) do
      Class.new(Grape::Entity) do
        expose(:kind) { |_| 'taggable' }
      end
    end

    let(:searchable_entity) do
      Class.new(Grape::Entity) do
        expose(:kind) { |_| 'searchable' }
      end
    end

    let(:featured_entity) do
      Class.new(Grape::Entity) do
        expose(:kind) { |_| 'featured' }
      end
    end

    it 'uses the module rule when it is the only match' do
      mod = taggable
      object_class = Class.new { include mod }
      entity = build_entity(taggable => taggable_entity)

      expect(entity.represent(feed(object_class.new), serializable: true)).to eq(
        items: [{ kind: 'taggable' }]
      )
    end

    it 'prefers the most specific module when modules include each other' do
      mod = featured
      object_class = Class.new { include mod }
      entity = build_entity(taggable => taggable_entity, featured => featured_entity)

      expect(entity.represent(feed(object_class.new), serializable: true)).to eq(
        items: [{ kind: 'featured' }]
      )
    end

    it 'raises an ambiguity error when incomparable modules match' do
      mod_a = taggable
      mod_b = searchable
      object_class = Class.new { include mod_a; include mod_b }
      entity = build_entity(taggable => taggable_entity, searchable => searchable_entity)

      expect { entity.represent(feed(object_class.new), serializable: true) }
        .to raise_error(Grape::Entity::PolymorphicMapping::AmbiguousPolymorphicMatch, /Ambiguous/)
    end

    it 'prefers a class rule over module rules' do
      mod = taggable
      object_class = Class.new(article_class) { include mod }
      entity = build_entity(article_class => article_entity, taggable => taggable_entity)

      expect(entity.represent(feed(object_class.new('Hello')), serializable: true)).to eq(
        items: [{ title: 'Hello', kind: 'article' }]
      )
    end
  end

  context 'with unknown types' do
    it 'uses the default presenter when declared' do
      entity = build_entity(article_class => article_entity, default: fallback_entity)

      expect(entity.represent(feed(video_class.new('http://example.com/v')), serializable: true)).to eq(
        items: [{ kind: 'fallback' }]
      )
    end

    it 'raises a fixed error when no default is declared' do
      entity = build_entity(article_class => article_entity)

      expect { entity.represent(feed(video_class.new('http://example.com/v')), serializable: true) }
        .to raise_error(Grape::Entity::PolymorphicMapping::UnmappedPolymorphicType, /No polymorphic presenter/)
    end
  end

  context 'with string presenter names' do
    it 'constantizes them lazily' do
      stub_const('PolymorphicArticleEntity', article_entity)
      entity = build_entity(article_class => 'PolymorphicArticleEntity')

      expect(entity.represent(feed(article_class.new('Hello')), serializable: true)).to eq(
        items: [{ title: 'Hello', kind: 'article' }]
      )
    end
  end

  context 'with invalid declarations' do
    it 'rejects combining :polymorphic with :using' do
      mapping = { article_class => article_entity }
      using = article_entity

      expect do
        Class.new(Grape::Entity) do
          expose :items, polymorphic: mapping, using: using
        end
      end.to raise_error(ArgumentError, /:polymorphic/)
    end

    it 'rejects non-hash mappings' do
      not_a_hash = article_entity

      expect do
        Class.new(Grape::Entity) do
          expose :items, polymorphic: not_a_hash
        end
      end.to raise_error(ArgumentError, /must be a Hash/)
    end

    it 'rejects non-module rule keys' do
      mapping = { 'Article' => article_entity }

      expect do
        Class.new(Grape::Entity) do
          expose :items, polymorphic: mapping
        end
      end.to raise_error(ArgumentError, /classes or modules/)
    end
  end

  context 'with entity inheritance' do
    let(:parent_entity) { build_entity(article_class => article_entity, default: fallback_entity) }

    it 'lets a subclass extend the mapping without polluting the parent' do
      extra = { video_class => video_entity }
      child_entity = Class.new(parent_entity) do
        extend_polymorphic :items, extra
      end

      model = feed(video_class.new('http://example.com/v'))

      expect(child_entity.represent(model, serializable: true)).to eq(
        items: [{ url: 'http://example.com/v', kind: 'video' }]
      )
      expect(parent_entity.represent(model, serializable: true)).to eq(
        items: [{ kind: 'fallback' }]
      )
    end

    it 'does not pollute sibling subclasses created before the extension' do
      sibling_entity = Class.new(parent_entity)
      extra = { video_class => video_entity }
      child_entity = Class.new(parent_entity) do
        extend_polymorphic :items, extra
      end

      model = feed(video_class.new('http://example.com/v'))

      expect(child_entity.represent(model, serializable: true)).to eq(
        items: [{ url: 'http://example.com/v', kind: 'video' }]
      )
      expect(sibling_entity.represent(model, serializable: true)).to eq(
        items: [{ kind: 'fallback' }]
      )
    end

    it 'lets a subclass override the default without polluting the parent' do
      other_fallback = Class.new(Grape::Entity) do
        expose(:kind) { |_| 'other-fallback' }
      end

      child_entity = Class.new(parent_entity) do
        extend_polymorphic :items, default: other_fallback
      end

      model = feed(video_class.new('http://example.com/v'))

      expect(child_entity.represent(model, serializable: true)).to eq(
        items: [{ kind: 'other-fallback' }]
      )
      expect(parent_entity.represent(model, serializable: true)).to eq(
        items: [{ kind: 'fallback' }]
      )
    end

    it 'raises when extending a missing exposure' do
      extra = { video_class => video_entity }

      expect do
        Class.new(parent_entity) do
          extend_polymorphic :missing, extra
        end
      end.to raise_error(ArgumentError, /No exposure found/)
    end

    it 'raises when extending a non-polymorphic exposure' do
      extra = { video_class => video_entity }

      expect do
        Class.new(Grape::Entity) do
          expose :items
          extend_polymorphic :items, extra
        end
      end.to raise_error(ArgumentError, /is not polymorphic/)
    end
  end

  context 'with a proc to fetch the value' do
    it 'dispatches the proc result' do
      mapping = { article_class => article_entity }
      entity = Class.new(Grape::Entity) do
        expose :first_item, polymorphic: mapping do |model, _|
          model.items.first
        end
      end

      expect(entity.represent(feed(article_class.new('Hello')), serializable: true)).to eq(
        first_item: { title: 'Hello', kind: 'article' }
      )
    end
  end
end
