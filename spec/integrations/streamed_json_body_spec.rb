# frozen_string_literal: true

require 'json'
require 'ostruct'

describe '::render_as_stream' do
  let(:vehicle) { OpenStruct.new(id: 10, make: 'Super Car') }
  let(:objects) do
    [
      OpenStruct.new(id: 1, name: 'First', quote: %q{she said "hi"}, newline: "a\nb",
                     vehicle:),
      OpenStruct.new(id: 2, name: 'Second', quote: 'plain', newline: "c\nd",
                     vehicle:)
    ]
  end
  let(:vehicle_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :make
    end
  end
  let(:blueprint) do
    vehicle_blueprint_const = vehicle_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
      field :quote
      field :newline
      association :vehicle, blueprint: vehicle_blueprint_const

      view :names_only do
        field :name
      end
    end
  end

  # A single-pass Enumerable that records how many elements have been pulled
  # and how many times it was closed.
  class CountingSource
    include Enumerable

    attr_reader :pull_count, :close_count

    def initialize(items)
      @items = items
      @pull_count = 0
      @close_count = 0
    end

    def each
      @items.each do |item|
        @pull_count += 1
        yield item
      end
    end

    def close
      @close_count += 1
    end
  end

  # Yields one element, then raises while iterating the second.
  class SourceThatRaises < CountingSource
    def each
      @items.each_with_index do |item, index|
        raise 'source exploded' if index >= 1

        @pull_count += 1
        yield item
      end
    end
  end

  # Has no #close method.
  class PlainSource
    include Enumerable

    def initialize(items)
      @items = items
    end

    def each
      @items.each { |item| yield item }
    end
  end

  class UpcasingTransformer < Blueprinter::Transformer
    def transform(hash, object, _options)
      hash[:name] = object.name.upcase
    end
  end

  class SourceWrappingExtension < Blueprinter::Extension
    attr_reader :calls

    def initialize
      @calls = 0
    end

    def pre_render(object, _blueprint, _view, _options)
      @calls += 1
      object
    end
  end

  after { reset_blueprinter_config! }

  def drain(body)
    chunks = []
    body.each { |chunk| chunks << chunk }
    chunks
  end

  describe 'return value' do
    it 'returns a Blueprinter::StreamedJsonBody' do
      body = blueprint.render_as_stream(CountingSource.new(objects))
      expect(body).to be_a(Blueprinter::StreamedJsonBody)
    end

    it 'rejects a source that is not an Enumerable' do
      expect { blueprint.render_as_stream(Object.new) }.
        to raise_error(ArgumentError, /must be an Enumerable/)
    end
  end

  describe 'lazy traversal' do
    it 'never reads the source before consumption starts' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      expect(source.pull_count).to eq(0)

      chunks = drain(body)
      expect(chunks.join).to eq(blueprint.render(objects))
      expect(source.pull_count).to eq(objects.size)
    end

    it 'only pulls the object needed for the current chunk' do
      source = CountingSource.new(objects)
      first_object_json = JSON.generate(
        id: 1, name: 'First', newline: "a\nb", quote: 'she said "hi"',
        vehicle: { id: 10, make: 'Super Car' }
      )
      second_object_json = JSON.generate(
        id: 2, name: 'Second', newline: "c\nd", quote: 'plain',
        vehicle: { id: 10, make: 'Super Car' }
      )
      snapshots = []

      body = blueprint.render_as_stream(source)
      body.each do |chunk|
        snapshots << [chunk, source.pull_count]
      end

      expect(snapshots).to eq(
        [
          ['[', 0],
          [first_object_json, 1],
          [',', 2],
          [second_object_json, 2],
          [']', 2]
        ]
      )
    end

    it 'pulls at most one element ahead when the consumer stops early' do
      source = CountingSource.new(Array.new(5) { |i| OpenStruct.new(id: i, name: i.to_s) })
      body = blueprint.render_as_stream(source, view: :names_only)

      seen_objects = 0
      body.each do |chunk|
        seen_objects += 1 if chunk.start_with?('{')
        break if seen_objects == 1
      end

      expect(source.pull_count).to eq(1)
    end

    it 'supports a blockless #each without touching the source' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)
      chunks_enum = body.each

      expect(chunks_enum).to be_a(Enumerator)
      expect(source.pull_count).to eq(0)

      expect(chunks_enum.next).to eq('[')
      expect(source.pull_count).to eq(0)

      expect { chunks_enum.next }.not_to raise_error
      expect(source.pull_count).to eq(1)
    end

    it 'can close the source after partially consuming the blockless enumerator' do
      source = CountingSource.new(Array.new(5) { |i| OpenStruct.new(id: i, name: i.to_s) })
      body = blueprint.render_as_stream(source, view: :names_only)
      chunks_enum = body.each

      3.times { chunks_enum.next }
      body.close

      expect(source.close_count).to eq(1)
    end
  end

  describe 'serialization rules' do
    it 'renders the same JSON as .render for the same collection' do
      chunks = drain(blueprint.render_as_stream(PlainSource.new(objects)))
      expect(chunks.join).to eq(blueprint.render(objects))
    end

    it 'honors the requested view' do
      names_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        view :names_only do
          field :name
        end
      end

      chunks = drain(
        names_blueprint.render_as_stream(
          PlainSource.new(objects), view: :names_only
        )
      )

      expect(JSON.parse(chunks.join)).to eq(
        [{ 'id' => 1, 'name' => 'First' }, { 'id' => 2, 'name' => 'Second' }]
      )
    end

    it 'raises UnknownView when a view is not defined on the blueprint' do
      expect { blueprint.render_as_stream(PlainSource.new(objects), view: :nope) }.
        to raise_error(Blueprinter::Errors::UnknownView)
    end

    it 'renders associations with their own blueprint' do
      chunks = drain(blueprint.render_as_stream(PlainSource.new(objects)))

      expect(JSON.parse(chunks.join).first['vehicle']).to eq(
        'id' => 10, 'make' => 'Super Car'
      )
    end

    it 'runs transformers once per element' do
      transformer_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
        transform UpcasingTransformer
      end

      chunks = drain(transformer_blueprint.render_as_stream(PlainSource.new(objects)))

      expect(JSON.parse(chunks.join).map { |row| row['name'] }).to eq(%w[FIRST SECOND])
    end

    it 'runs pre_render extensions a single time for the collection' do
      extension = SourceWrappingExtension.new
      Blueprinter.configuration.extensions << extension

      simple_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        field :name
      end

      drain(simple_blueprint.render_as_stream(CountingSource.new(objects)))

      expect(extension.calls).to eq(1)
    end

    it 'escapes strings and separates elements with commas' do
      chunks = drain(blueprint.render_as_stream(PlainSource.new(objects)))

      expect(chunks.count(',')).to eq(1)
      joined = chunks.join
      expect(joined).to include('she said \"hi\"')
      expect(joined).to include('a\nb')
      expect(JSON.parse(joined).size).to eq(2)
    end

    it 'renders an empty collection as an empty array' do
      chunks = drain(blueprint.render_as_stream(PlainSource.new([])))

      expect(chunks).to eq(['[', ']'])
      expect(JSON.parse(chunks.join)).to eq([])
    end
  end

  describe ':root and :meta' do
    it 'rejects :root before reading the source' do
      source = CountingSource.new(objects)

      expect { blueprint.render_as_stream(source, root: :widgets) }.
        to raise_error(Blueprinter::Errors::StreamingRootUnsupported)
      expect(source.pull_count).to eq(0)
      expect(source.close_count).to eq(0)
    end

    it 'rejects :meta before reading the source' do
      source = CountingSource.new(objects)

      expect { blueprint.render_as_stream(source, meta: { total: 2 }) }.
        to raise_error(Blueprinter::Errors::StreamingMetaUnsupported)
      expect(source.pull_count).to eq(0)
      expect(source.close_count).to eq(0)
    end

    it 'rejects :root even when its value is nil' do
      expect { blueprint.render_as_stream(PlainSource.new(objects), root: nil) }.
        to raise_error(Blueprinter::Errors::StreamingRootUnsupported)
    end

    it 'rejects :meta even when its value is nil' do
      expect { blueprint.render_as_stream(PlainSource.new(objects), meta: nil) }.
        to raise_error(Blueprinter::Errors::StreamingMetaUnsupported)
    end

    it 'rejects an unknown view before reading the source' do
      source = CountingSource.new(objects)

      expect { blueprint.render_as_stream(source, view: :missing) }.
        to raise_error(Blueprinter::Errors::UnknownView)
      expect(source.pull_count).to eq(0)
    end
  end

  describe 'closing the source' do
    it 'closes the source exactly once after a normal finish' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      drain(body)
      expect(source.close_count).to eq(1)

      body.close
      expect(source.close_count).to eq(1)
      expect(body).to be_closed
    end

    it 'closes the source exactly once when the consumer stops early' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      body.each { break }
      expect(source.close_count).to eq(1)

      body.close
      expect(source.close_count).to eq(1)
    end

    it 'closes exactly once when the consumer raises while receiving a chunk' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      expect do
        body.each { raise 'consumer gave up' }
      end.to raise_error('consumer gave up')
      expect(source.close_count).to eq(1)
    end

    it 'closes the source exactly once when iteration raises after bytes are sent' do
      source = SourceThatRaises.new(objects)
      body = blueprint.render_as_stream(source)
      chunks = []

      expect { body.each { |chunk| chunks << chunk } }.
        to raise_error('source exploded')

      expect(source.close_count).to eq(1)
      body.close
      expect(source.close_count).to eq(1)
    end

    it 'closes the source exactly once when the second object fails to serialize' do
      bad_object = Object.new
      def bad_object.id
        raise 'boom from second item'
      end

      source = CountingSource.new([objects.first, bad_object])
      body = blueprint.render_as_stream(source)
      chunks = []

      expect { body.each { |chunk| chunks << chunk } }.
        to raise_error('boom from second item')

      # The first element plus its trailing comma were emitted, but the array
      # was never terminated.
      expect(chunks.join).to eq(
        '[' + JSON.generate(id: 1, name: 'First', newline: "a\nb",
                                   quote: 'she said "hi"',
                                   vehicle: { id: 10, make: 'Super Car' }) + ','
      )
      expect(chunks.join).not_to end_with(']')
      expect { JSON.parse(chunks.join) }.to raise_error(JSON::ParserError)
      expect(source.close_count).to eq(1)
    end

    it 'does not call close on sources that do not support it' do
      body = blueprint.render_as_stream(PlainSource.new(objects))
      expect { drain(body) }.not_to raise_error
    end

    it 'can be closed before consumption and then refuses to stream' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      body.close
      expect(source.close_count).to eq(1)
      expect(source.pull_count).to eq(0)

      expect { drain(body) }.to raise_error(IOError, /closed/)
      expect(source.close_count).to eq(1)
    end

    it 'refuses to be consumed a second time' do
      source = CountingSource.new(objects)
      body = blueprint.render_as_stream(source)

      drain(body)
      expect { drain(body) }.to raise_error(IOError, /already been consumed/)
    end
  end
end
