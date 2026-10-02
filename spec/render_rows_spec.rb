# frozen_string_literal: true

require 'ostruct'

describe '::render_rows' do
  # Models for a small order-export domain:
  # Order -> Customer (singular), Order -> Items (collection),
  # Item -> Product (singular), Customer -> Tags (second, undeclared collection).
  let(:customer_a) do
    OpenStruct.new(id: 10, name: 'Acme', tier: 'gold',
                   tags: [OpenStruct.new(label: 'vip')])
  end
  let(:customer_b) do
    OpenStruct.new(id: 11, name: 'Beta', tier: nil, tags: [])
  end
  let(:customer_c) { OpenStruct.new(id: 12, name: 'Gamma', tier: 'silver', tags: nil) }

  let(:item_a1) { OpenStruct.new(sku: 'A-1', price: 9.99, product: OpenStruct.new(name: 'Widget')) }
  let(:item_a2) { OpenStruct.new(sku: 'A-2', price: 19.99, product: OpenStruct.new(name: 'Gadget')) }
  let(:item_b1) { OpenStruct.new(sku: 'B-1', price: 5.0, product: OpenStruct.new(name: 'Gizmo')) }

  let(:order_a) { OpenStruct.new(id: 1, number: 'ORD-1', confidential: 'TOPSECRET', customer: customer_a, items: [item_a1, item_a2]) }
  let(:order_b) { OpenStruct.new(id: 2, number: 'ORD-2', confidential: 'TOPSECRET', customer: customer_b, items: [item_b1]) }
  let(:order_c_empty) { OpenStruct.new(id: 3, number: 'ORD-3', customer: customer_c, items: []) }
  let(:order_c_nil) { OpenStruct.new(id: 4, number: 'ORD-4', customer: customer_c, items: nil) }

  let(:product_blueprint) do
    Class.new(Blueprinter::Base) do
      field :name
    end
  end

  let(:item_blueprint) do
    product_bp = product_blueprint
    Class.new(Blueprinter::Base) do
      field :sku
      field :price
      association :product, blueprint: product_bp
    end
  end

  let(:customer_blueprint) do
    Class.new(Blueprinter::Base) do
      field :name
      field :tier, if: ->(_field, customer, _options) { !customer.tier.nil? }
    end
  end

  let(:order_blueprint) do
    item_bp = item_blueprint
    customer_bp = customer_blueprint
    Class.new(Blueprinter::Base) do
      identifier :id
      field :number
      association :customer, blueprint: customer_bp
      association :items, blueprint: item_bp
    end
  end

  let(:columns) do
    [
      :id,
      :number,
      %i[customer name],
      %i[items sku],
      ['items.price'],
      %i[items product name]
    ]
  end

  subject(:result) do
    order_blueprint.render_rows(
      [order_a, order_b],
      columns:,
      collection: :items
    )
  end

  describe 'return value' do
    it 'returns column names and positional rows without writing a file' do
      expect(result.keys).to eq(%i[columns rows])
      expect(result[:columns]).to eq(%i[id number name sku price name])
    end

    it 'accepts columns and collection as positional arguments' do
      positional = order_blueprint.render_rows(
        [order_a],
        [:id, %i[items sku]],
        :items
      )
      expect(positional[:rows]).to eq([[1, 'A-1'], [1, 'A-2']])
    end

    it 'raises ArgumentError when columns or collection are missing' do
      expect { order_blueprint.render_rows([order_a]) }.to raise_error(ArgumentError, /column paths/)
      expect do
        order_blueprint.render_rows([order_a], columns: [:id])
      end.to raise_error(ArgumentError, /collection/)
    end
  end

  describe 'row expansion' do
    it 'creates one row per item, repeating parent columns in order' do
      expect(result[:rows]).to eq(
        [
          [1, 'ORD-1', 'Acme', 'A-1', 9.99, 'Widget'],
          [1, 'ORD-1', 'Acme', 'A-2', 19.99, 'Gadget'],
          [2, 'ORD-2', 'Beta', 'B-1', 5.0, 'Gizmo']
        ]
      )
    end

    it 'accepts a single parent object' do
      single = order_blueprint.render_rows(
        order_a,
        columns: [:id, %i[items sku]],
        collection: :items
      )
      expect(single[:rows]).to eq([[1, 'A-1'], [1, 'A-2']])
    end

    it 'uses the current element itself when a column path ends at the collection' do
      simple_item_blueprint = Class.new(Blueprinter::Base) do
        identifier :sku
      end
      simple_order_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :items, blueprint: simple_item_blueprint
      end

      payload = simple_order_blueprint.render_rows(
        [order_a],
        columns: [:id, :items],
        collection: :items
      )

      expect(payload[:columns]).to eq(%i[id items])
      expect(payload[:rows]).to eq([[1, { sku: 'A-1' }], [1, { sku: 'A-2' }]])
    end

    it 'supports a dot-separated collection path through a singular association' do
      order_with_nested = OpenStruct.new(
        id: 9,
        cart: OpenStruct.new(items: [OpenStruct.new(sku: 'X-1')])
      )
      line_blueprint = Class.new(Blueprinter::Base) { field :sku }
      cart_blueprint = Class.new(Blueprinter::Base) do
        association :items, blueprint: line_blueprint
      end
      root_blueprint = Class.new(Blueprinter::Base) do
        identifier :id
        association :cart, blueprint: cart_blueprint
      end

      nested = root_blueprint.render_rows(
        order_with_nested,
        columns: [:id, %i[cart items sku]],
        collection: 'cart.items'
      )
      expect(nested[:rows]).to eq([[9, 'X-1']])
    end
  end

  describe 'empty and nil collections' do
    let(:orders) { [order_a, order_c_empty, order_c_nil] }

    it 'keeps a blank-detail row per parent by default' do
      rows = order_blueprint.render_rows(
        orders,
        columns: [:id, %i[items sku]],
        collection: :items
      )[:rows]

      expect(rows).to eq(
        [
          [1, 'A-1'],
          [1, 'A-2'],
          [3, nil],
          [4, nil]
        ]
      )
    end

    it 'omits the parent when on_empty is :omit' do
      rows = order_blueprint.render_rows(
        orders,
        columns: [:id, %i[items sku]],
        collection: :items,
        on_empty: :omit
      )[:rows]

      expect(rows).to eq([[1, 'A-1'], [1, 'A-2']])
    end
  end

  describe 'conditional fields' do
    it 'leaves a blank cell without shifting the columns that follow' do
      rows = order_blueprint.render_rows(
        [order_b],
        columns: [:id, %i[customer tier], %i[customer name], %i[items sku]],
        collection: :items
      )[:rows]

      expect(rows).to eq([[2, nil, 'Beta', 'B-1']])
    end
  end

  describe 'path validation' do
    it 'raises InvalidRowPath for an unknown field before rendering' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id, %i[nope sku]],
          collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /nope/)
    end

    it 'raises when a column crosses a second, undeclared collection' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id, %i[customer tags label]],
          collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /tags/)
    end

    it 'raises when a column ends at an independent collection association' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id, %i[customer tags]],
          collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /tags/)
    end

    it 'raises when the declared collection is not array-like' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id, %i[customer name]],
          collection: :customer
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /array-like/)
    end

    it 'raises InvalidRowPath for the empty collection through a singular path' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id, %i[customer items sku]],
          collection: :items
        )
      end.to raise_error(Blueprinter::Errors::InvalidRowPath, /items/)
    end

    it 'raises UnknownView for a view that does not exist' do
      expect do
        order_blueprint.render_rows(
          [order_a],
          columns: [:id],
          collection: :items,
          view: :missing
        )
      end.to raise_error(Blueprinter::Errors::UnknownView)
    end
  end

  describe 'views, transformers and field extraction' do
    let(:read_counts) { Hash.new(0) }

    let(:counting_blueprint) do
      counts = read_counts
      Class.new(Blueprinter::Base) do
        identifier :id
        field :number do |order|
          counts[:number] += 1
          order.number
        end
        field :confidential, if: ->(_field, _order, options) { options[:show_internal] }
        association :items, blueprint: Class.new(Blueprinter::Base) { field :sku }

        transformer_class = Class.new(Blueprinter::Transformer) do
          define_method(:transform) do |hash, object, _options|
            hash[:number_upcase] = object.number.upcase
          end
        end
        transform transformer_class
      end
    end

    it 'extracts parent fields only once even when several rows are produced' do
      counting_blueprint.render_rows(
        [order_a, order_b],
        columns: [:id, :number, %i[items sku]],
        collection: :items
      )

      expect(read_counts[:number]).to eq(2)
    end

    it 'reads every parent object and singular association exactly once' do
      counts = Hash.new(0)
      tracking_customer_bp = Class.new(Blueprinter::Base) do
        c = counts
        field :name do |customer|
          c[:customer_name] += 1
          customer.name
        end
      end
      tracking_order_bp = Class.new(Blueprinter::Base) do
        c = counts
        customer_bp = tracking_customer_bp
        field :number do |order|
          c[:order_number] += 1
          order.number
        end
        association :customer, blueprint: customer_bp
        association :items, blueprint: Class.new(Blueprinter::Base) { field :sku }
      end

      tracking_order_bp.render_rows(
        [order_a, order_b],
        columns: [:number, %i[customer name], %i[items sku]],
        collection: :items
      )

      expect(counts[:order_number]).to eq(2)
      expect(counts[:customer_name]).to eq(2)
    end

    it 'keeps hidden fields as blank cells and applies transformers to values' do
      rows = counting_blueprint.render_rows(
        [order_a],
        columns: [:id, :confidential, :number_upcase, %i[items sku]],
        collection: :items
      )[:rows]

      expect(rows).to eq(
        [
          [1, nil, 'ORD-1', 'A-1'],
          [1, nil, 'ORD-1', 'A-2']
        ]
      )
    end

    it 'passes extra render options through to fields and associations' do
      rows = counting_blueprint.render_rows(
        [order_a],
        columns: [:id, :confidential, %i[items sku]],
        collection: :items,
        show_internal: true
      )[:rows]

      expect(rows).to eq([[1, 'TOPSECRET', 'A-1'], [1, 'TOPSECRET', 'A-2']])
    end
  end

  describe 'output aliases' do
    let(:aliased_blueprint) do
      customer_bp = customer_blueprint
      item_bp = item_blueprint
      Class.new(Blueprinter::Base) do
        identifier :id, name: :order_id
        association :customer, name: :client, blueprint: customer_bp
        association :items, name: :lines, blueprint: item_bp
      end
    end

    it 'resolves paths using output aliases' do
      payload = aliased_blueprint.render_rows(
        [order_a],
        columns: [:order_id, %i[client name], %i[lines sku]],
        collection: :lines
      )

      expect(payload[:columns]).to eq(%i[order_id name sku])
      expect(payload[:rows]).to eq([[1, 'Acme', 'A-1'], [1, 'Acme', 'A-2']])
    end
  end

  describe 'regular nested rendering' do
    it 'still returns the original nested hash structure' do
      expect(order_blueprint.render_as_hash([order_a, order_b])).to eq(
        [
          {
            id: 1,
            number: 'ORD-1',
            customer: { name: 'Acme', tier: 'gold' },
            items: [
              { sku: 'A-1', price: 9.99, product: { name: 'Widget' } },
              { sku: 'A-2', price: 19.99, product: { name: 'Gadget' } }
            ]
          },
          {
            id: 2,
            number: 'ORD-2',
            customer: { name: 'Beta' },
            items: [
              { sku: 'B-1', price: 5.0, product: { name: 'Gizmo' } }
            ]
          }
        ]
      )
    end

    it 'still renders JSON as before' do
      json = order_blueprint.render(order_a)
      expect(JSON.parse(json)['number']).to eq('ORD-1')
    end
  end
end
