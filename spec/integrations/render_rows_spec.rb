# frozen_string_literal: true

require 'ostruct'

describe '::render_rows' do
  item_total_transformer = Class.new(Blueprinter::Transformer) do
    def transform(hash, item, _options)
      hash[:total] = item.quantity * 10
    end
  end

  let(:product_blueprint) do
    Class.new(Blueprinter::Base) do
      identifier :id
      field :name
    end
  end

  let(:tag_blueprint) do
    Class.new(Blueprinter::Base) do
      field :label
    end
  end

  let(:item_blueprint) do
    product_bp = product_blueprint
    tag_bp = tag_blueprint
    Class.new(Blueprinter::Base) do
      field :sku
      field :quantity
      field :internal_price, if: ->(_field, _item, options) { options[:show_cost] }
      association :product, blueprint: product_bp
      association :tags, blueprint: tag_bp
      transform item_total_transformer
    end
  end

  let(:customer_blueprint) do
    Class.new(Blueprinter::Base) do
      field :name
    end
  end

  let(:payment_blueprint) do
    Class.new(Blueprinter::Base) do
      field :amount
    end
  end

  let(:order_blueprint) do
    item_bp = item_blueprint
    customer_bp = customer_blueprint
    payment_bp = payment_blueprint
    Class.new(Blueprinter::Base) do
      field :code, name: :number do |order|
        order.reads += 1
        order.code
      end
      field :secret_note, if: ->(_field, _order, _options) { false }
      field :export_only
      association :customer, blueprint: customer_bp
      association :items, blueprint: item_bp
      association :payments, blueprint: payment_bp

      view :export do
        field :shipping_method
      end
    end
  end

  def build_item(sku, quantity, tags: [])
    OpenStruct.new(
      sku: sku,
      quantity: quantity,
      internal_price: 42,
      product: OpenStruct.new(id: 1, name: "Product #{sku}"),
      tags: tags.map { |label| OpenStruct.new(label: label) }
    )
  end

  let(:order_with_two_items) do
    OpenStruct.new(
      code: 'O-1',
      reads: 0,
      secret_note: 'do not show',
      export_only: 'EXP-1',
      shipping_method: 'air',
      customer: OpenStruct.new(name: 'Acme'),
      items: [build_item('A', 2, tags: ['x']), build_item('B', 1)],
      payments: [OpenStruct.new(amount: 100)]
    )
  end

  let(:order_with_empty_items) do
    OpenStruct.new(
      code: 'O-2',
      reads: 0,
      secret_note: 'hidden',
      export_only: 'EXP-2',
      shipping_method: 'sea',
      customer: OpenStruct.new(name: 'Globex'),
      items: nil,
      payments: []
    )
  end

  let(:orders) { [order_with_two_items, order_with_empty_items] }

  let(:columns) do
    [
      :number,
      :'customer.name',
      :'items.product.name',
      :'items.sku',
      :'items.quantity',
      :'items.total',
      :secret_note
    ]
  end

  subject(:result) do
    order_blueprint.render_rows(orders, columns:, collection: :items)
  end

  it 'returns the ordered column names without writing any file' do
    expect(result[:columns]).to eq(
      ['number', 'customer.name', 'items.product.name', 'items.sku',
       'items.quantity', 'items.total', 'secret_note']
    )
  end

  it 'expands one row per item, repeating parent columns, and keeps row order' do
    expect(result[:rows]).to eq(
      [
        ['O-1', 'Acme', 'Product A', 'A', 2, 20, nil],
        ['O-1', 'Acme', 'Product B', 'B', 1, 10, nil],
        ['O-2', 'Globex', nil, nil, nil, nil, nil]
      ]
    )
  end

  it 'extracts each parent field only once even when multiple rows are generated' do
    result

    expect(order_with_two_items.reads).to eq(1)
    expect(order_with_empty_items.reads).to eq(1)
  end

  it 'leaves blank cells for condition-hidden fields without shifting later columns' do
    result[:rows].each do |row|
      expect(row.last).to be_nil
      expect(row.length).to eq(columns.length)
    end
  end

  it 'crosses plain object associations on both sides of the collection' do
    rows = result[:rows]

    expect(rows[0][1]).to eq('Acme')
    expect(rows[0][2]).to eq('Product A')
  end

  it 'runs detail fields through transformers' do
    expect(result[:rows][0][5]).to eq(20)
  end

  context 'when the collection is empty or nil' do
    it 'keeps one row with blank detail cells by default' do
      expect(result[:rows].last).to eq(['O-2', 'Globex', nil, nil, nil, nil, nil])
      expect(result[:rows].length).to eq(3)
    end

    it 'omits the parent when on_empty_collection is :omit' do
      omitted = order_blueprint.render_rows(
        orders, columns:, collection: :items, on_empty_collection: :omit
      )

      expect(omitted[:rows].map(&:first)).to eq(['O-1', 'O-1'])
    end

    it 'raises on an unknown on_empty_collection value' do
      expect do
        order_blueprint.render_rows(orders, columns:, collection: :items, on_empty_collection: :nope)
      end.to raise_error(ArgumentError, /on_empty_collection/)
    end
  end

  describe 'path errors' do
    it 'raises when a column crosses a second, undeclared collection at the parent level' do
      expect do
        order_blueprint.render_rows(
          orders, columns: [:'payments.amount'], collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /payments\.amount.*undeclared collection/)
    end

    it 'raises when a column crosses a collection nested inside collection elements' do
      expect do
        order_blueprint.render_rows(
          orders, columns: [:'items.tags.label'], collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /items\.tags\.label.*undeclared collection/)
    end

    it 'raises for an unknown column alias even before inspecting the data' do
      expect do
        order_blueprint.render_rows(orders, columns: [:nope], collection: :items)
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /nope/)
    end

    it 'raises when the declared collection path is not an association' do
      expect do
        order_blueprint.render_rows(orders, columns: [:number], collection: :number)
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /number.*not an association/)
    end

    it 'raises when the declared association does not render a collection' do
      expect do
        order_blueprint.render_rows(orders, columns: [:'customer.name'], collection: :customer)
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /customer.*does not reference a collection/)
    end

    it 'raises for an unknown collection alias even when the data is nil' do
      expect do
        order_blueprint.render_rows([order_with_empty_items], columns: [:number], collection: :nope)
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /nope/)
    end
  end

  describe 'views and conditions' do
    it 'uses the declared view' do
      rows = order_blueprint.render_rows(
        orders,
        columns: [:number, :shipping_method],
        collection: :items,
        view: :export
      )

      expect(rows[:rows].first).to eq(['O-1', 'air'])
    end

    it 'raises for a field that only exists in another view' do
      expect do
        order_blueprint.render_rows(
          orders, columns: [:shipping_method], collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /shipping_method/)
    end

    it 'passes render options through to detail conditions' do
      rows = order_blueprint.render_rows(
        orders,
        columns: [:'items.sku', :'items.internal_price'],
        collection: :items,
        show_cost: true
      )

      expect(rows[:rows].first).to eq(['A', 42])
      expect(rows[:rows].last).to eq([nil, nil])
    end
  end

  describe 'path formats' do
    it 'accepts strings and arrays of aliases' do
      rows = order_blueprint.render_rows(
        order_with_two_items,
        columns: [['items', 'sku'], 'customer.name'],
        collection: 'items'
      )

      expect(rows[:columns]).to eq(['items.sku', 'customer.name'])
      expect(rows[:rows].first).to eq(['A', 'Acme'])
    end

    it 'supports a column pointing at the collection itself' do
      rows = order_blueprint.render_rows(
        order_with_two_items, columns: [:number, :items], collection: :items
      )

      expect(rows[:rows].first).to match(['O-1', hash_including(sku: 'A')])
    end
  end

  describe 'without a declared collection' do
    it 'renders one row per object and still rejects crossed collections' do
      rows = order_blueprint.render_rows(orders, columns: [:number, :'customer.name'])

      expect(rows[:rows]).to eq([['O-1', 'Acme'], ['O-2', 'Globex']])

      expect do
        order_blueprint.render_rows(orders, columns: [:'items.sku'])
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /undeclared collection/)
    end
  end

  describe 'plain nested rendering' do
    it 'still returns the original nested structure' do
      expect(order_blueprint.render_as_hash(order_with_two_items)).to include(
        number: 'O-1',
        customer: { name: 'Acme' },
        items: [
          hash_including(sku: 'A', quantity: 2, product: hash_including(name: 'Product A')),
          hash_including(sku: 'B', quantity: 1)
        ]
      )
    end
  end
end
