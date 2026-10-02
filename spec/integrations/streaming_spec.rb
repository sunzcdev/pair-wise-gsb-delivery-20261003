# frozen_string_literal: true

require 'ostruct'

# A finite, single-pass enumerable that counts how many elements have been
# pulled from it and how many times it has been closed.
class CountingEnumerable
  include Enumerable

  attr_reader :pulled, :close_count

  def initialize(items)
    @items = items
    @pulled = 0
    @close_count = 0
  end

  def each
    return enum_for(:each) unless block_given?

    @items.each do |item|
      @pulled += 1
      yield item
    end
  end

  def close
    @close_count += 1
  end
end

# A single-pass enumerable that raises when its second element is requested.
class ExplodingEnumerable < CountingEnumerable
  class Boom < StandardError; end

  def each
    return enum_for(:each) unless block_given?

    @items.each_with_index do |item, index|
      raise Boom if index == 1

      @pulled += 1
      yield item
    end
  end
end

describe '::render_stream' do
  let(:blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
    end
  end
  let(:items) { [OpenStruct.new(id: 1, name: 'one'), OpenStruct.new(id: 2, name: 'two')] }
  let(:source) { CountingEnumerable.new(items) }
  let(:body) { blueprint.render_stream(source) }

  it 'does not read the source before consumption starts' do
    body
    expect(source.pulled).to eq(0)
  end

  it 'serializes only the current object on each advance' do
    chunks = body.each
    expect(chunks.next).to eq('[')
    expect(source.pulled).to eq(0)
    expect(chunks.next).to eq('{"id":1,"name":"one"}')
    expect(source.pulled).to eq(1)
    expect(chunks.next).to eq(',{"id":2,"name":"two"}')
    expect(source.pulled).to eq(2)
    expect(chunks.next).to eq(']')
    expect { chunks.next }.to raise_error(StopIteration)
  end

  it 'produces the same JSON as render' do
    expect(body.each.to_a.join).to eq(blueprint.render(items))
  end

  it 'escapes strings correctly' do
    tricky_items = [OpenStruct.new(id: 1, name: %(a "quoted" \\ name)), OpenStruct.new(id: 2, name: "tab\there")]
    json = blueprint.render_stream(CountingEnumerable.new(tricky_items)).each.to_a.join
    expect(json).to eq(blueprint.render(tricky_items))
    expect(JSON.parse(json).map { |entry| entry['name'] }).to eq(tricky_items.map(&:name))
  end

  it 'renders an empty collection as an empty array' do
    empty_source = CountingEnumerable.new([])
    expect(blueprint.render_stream(empty_source).each.to_a.join).to eq('[]')
    expect(empty_source.close_count).to eq(1)
  end

  it 'closes the source exactly once when fully consumed' do
    body.each.to_a
    body.close
    expect(source.close_count).to eq(1)
  end

  it 'closes the source exactly once when closed early' do
    chunks = body.each
    chunks.next
    chunks.next
    body.close
    body.close
    expect(source.close_count).to eq(1)
  end

  context 'when the source raises after chunks have been emitted' do
    let(:source) { ExplodingEnumerable.new(items) }

    it 'propagates the error without appending a closing bracket and closes the source once' do
      chunks = []
      expect { body.each { |chunk| chunks << chunk } }.to raise_error(ExplodingEnumerable::Boom)
      expect(chunks).to eq(['[', '{"id":1,"name":"one"}'])
      expect(source.close_count).to eq(1)
    end
  end

  context 'with :root or :meta' do
    it 'raises UnsupportedStreamOption before reading the source' do
      expect { blueprint.render_stream(source, root: :data) }.
        to raise_error(Blueprinter::Errors::UnsupportedStreamOption, /does not support/)
      expect { blueprint.render_stream(source, meta: { total: 2 }) }.
        to raise_error(Blueprinter::Errors::UnsupportedStreamOption, /does not support/)
      expect(source.pulled).to eq(0)
    end

    it 'remains rescuable as BlueprinterError' do
      expect { blueprint.render_stream(source, root: :data) }.to raise_error(Blueprinter::BlueprinterError)
    end
  end

  context 'with an undefined view' do
    it 'raises UnknownView before reading the source' do
      expect { blueprint.render_stream(source, view: :missing) }.
        to raise_error(Blueprinter::Errors::UnknownView, "View 'missing' is not defined")
      expect(source.pulled).to eq(0)
    end
  end

  context 'with views, associations and transformers' do
    let(:child_blueprint) do
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end
    end
    let(:upcase_transformer) do
      Class.new(Blueprinter::Transformer) do
        def transform(hash, _object, _options)
          hash[:upcased] = hash[:name].upcase
        end
      end
    end
    let(:blueprint) do
      child = child_blueprint
      transformer = upcase_transformer
      Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        transform(transformer)

        view :extended do
          field :extra
          association :children, blueprint: child
        end
      end
    end
    let(:items) do
      [
        OpenStruct.new(id: 1, name: 'one', extra: 'x', children: [OpenStruct.new(id: 3, name: 'three')]),
        OpenStruct.new(id: 2, name: 'two', extra: 'y', children: [])
      ]
    end

    it 'renders identically to render for each view' do
      expect(blueprint.render_stream(CountingEnumerable.new(items)).each.to_a.join).to eq(blueprint.render(items))
      expect(blueprint.render_stream(CountingEnumerable.new(items), view: :extended).each.to_a.join).
        to eq(blueprint.render(items, view: :extended))
    end
  end
end
