# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe Types do
    it 'builds shapes from plain ruby data' do
      shape = Types.build(
        'name' => :string,
        'email?' => :string,
        'tags' => [:string],
        'score' => [:or, :number, :null]
      )
      expect(shape).to be_a(Types::ObjectType)
      expect(shape.fields['name']).to eq(Types::STRING)
      expect(shape.optional?('email')).to be(true)
      expect(shape.fields['tags']).to eq(Types.array_of(:string))
      expect(shape.fields['score']).to eq(Types.union(Types::NUMBER, Types::NULL))
    end

    it 'prints shapes readably' do
      shape = Types.object('name' => :string, 'email?' => :string)
      expect(shape.to_s).to eq('object{name: string, email?: string}')
      expect(Types.array_of(:string).to_s).to eq('array<string>')
      expect(Types.optional(:number).to_s).to eq('number | null')
    end

    it 'intersects disjoint shapes to nil' do
      expect(Types.intersect(Types::STRING, Types::NUMBER)).to be_nil
      expect(Types.intersect(Types.array_of(:string), Types.array_of(:number))).to be_nil
      expect(Types.intersect(Types.optional(:string), Types::STRING)).to eq(Types::STRING)
      expect(Types.intersect(Types::ANY, Types::NUMBER)).to eq(Types::NUMBER)
    end
  end

  describe '.check_types' do
    let(:shape) do
      Types.object(
        'name' => :string,
        'email' => [:maybe, :string],
        'age' => :number,
        'tags' => [:string],
        'scores' => [[:or, :number, :string]],
        'address' => [:maybe, { 'city' => :string }],
        'users' => [{ 'kind' => :string, 'score' => [:maybe, :number] }]
      )
    end

    def check(expression, input = nil)
      JMESPath.check_types(expression, input || shape)
    end

    describe 'output type inference' do
      it 'infers field types' do
        expect(check('name').output_type).to eq(Types::STRING)
        expect(check('age').output_type).to eq(Types::NUMBER)
      end

      it 'marks optional fields nullable' do
        expect(check('email').output_type).to eq(Types.optional(:string))
      end

      it 'infers nested field types through unions' do
        expect(check('address.city').output_type).to eq(Types.optional(:string))
      end

      it 'infers projection element types' do
        expect(check('tags[*]').output_type).to eq(Types.array_of(:string))
        expect(check('users[].kind').output_type).to eq(Types.array_of(:string))
      end

      it 'drops nulls from projection results' do
        expect(check('users[].score').output_type).to eq(Types.array_of(:number))
      end

      it 'keeps nulls in multi-select lists' do
        expect(check('users[].[score]').output_type)
          .to eq(Types.array_of(Types.array_of(Types.optional(:number))))
      end

      it 'infers function return types' do
        expect(check('length(name)').output_type).to eq(Types::NUMBER)
        expect(check('keys(@)').output_type).to eq(Types.array_of(:string))
        expect(check('sum(tags)').output_type).to eq(Types::NUMBER)
      end

      it 'infers values() element types from known object fields' do
        result = check('values(@)', Types.object('a' => :string, 'b' => :number))
        expect(result.output_type).to eq(Types.array_of(Types.union(:string, :number)))
      end

      it 'treats undeclared fields as null with a note' do
        result = check('missing')
        expect(result.output_type).to eq(Types::NULL)
        expect(result.output_type.note).to include('not declared')
      end

      it 'treats undeclared fields as any on open objects' do
        open = Types.object({ 'name' => :string }, open: true)
        expect(check('anything', open).output_type).to eq(Types::ANY)
      end
    end

    describe 'compile-time errors' do
      it 'catches provable argument type errors' do
        result = check('abs(email)')
        expect(result.ok?).to be(false)
        error = result.errors.first
        expect(error.message).to include('abs()')
        expect(error.message).to include('number')
        expect(error.message).to include('string | null')
        expect(error.node).to eq('abs(email)')
      end

      it 'explains the inference behind an error' do
        result = check('length(tags[0][0])')
        expect(result.ok?).to be(false)
        expect(result.errors.first.reason).to include('string values have no fields')
      end

      it 'catches array element type mismatches' do
        result = check('sum(tags)')
        expect(result.ok?).to be(false)
        expect(result.errors.first.message).to include('array<number>, got array<string>')
      end

      it 'catches arity errors' do
        result = check('abs(name, age)')
        expect(result.ok?).to be(false)
        expect(result.errors.first.message).to include('expects 1 argument(s), got 2')
      end

      it 'catches errors inside filter projections' do
        result = check('users[*].abs(kind)')
        expect(result.ok?).to be(false)
      end

      it 'raises through assert_types!' do
        expect { JMESPath.assert_types!('abs(email)', shape) }
          .to raise_error(Errors::TypeCheckError, /abs\(\)/)
      end
    end

    describe 'unknown shapes are never errors' do
      it 'passes any-typed arguments' do
        expect(check('abs(@)', :any).ok?).to be(true)
      end

      it 'passes partially overlapping types (mixed arrays)' do
        expect(check('sum(scores)').ok?).to be(true)
        expect(check("join(',', scores)").ok?).to be(true)
      end

      it 'passes optional fields when null is acceptable' do
        expect(check('length(email)').ok?).to be(true)
      end

      it 'passes undeclared fields on open objects' do
        open = Types.object({ 'name' => :string }, open: true)
        expect(check('abs(unknown_field)', open).ok?).to be(true)
      end
    end

    describe 'filter narrowing' do
      it 'narrows optional fields with != `null`' do
        result = check('users[?score != `null`].[score]')
        expect(result.output_type).to eq(Types.array_of(Types.array_of(:number)))
      end

      it 'narrows equality with `null` to null' do
        result = check('users[?score == `null`].[score] | abs([0])')
        expect(result.ok?).to be(false)
        expect(result.errors.first.message).to include('got array<null>')
      end

      it 'records the filter as the reason for narrowed types' do
        result = check('users[?score == `null`].[score] | abs([0])')
        expect(result.errors.first.reason).to include('narrowed')
        expect(result.errors.first.reason).to include('score == null')
      end

      it 'narrows number comparisons' do
        input = Types.object('items' => [{ 'v' => [:or, :number, :string] }])
        result = check('items[?v > `10`].[v]', input)
        expect(result.output_type).to eq(Types.array_of(Types.array_of(:number)))
      end

      it 'narrows through && conditions' do
        result = check('users[?score != `null` && kind == `"admin"`].[score]')
        expect(result.output_type).to eq(Types.array_of(Types.array_of(:number)))
      end
    end

    describe 'comparison with actual runtime behavior' do
      let(:data_full) do
        {
          'name' => 'alice', 'email' => 'a@b.c', 'age' => 30,
          'tags' => %w[x y], 'scores' => [1, 2, 3],
          'address' => { 'city' => 'Shanghai' },
          'users' => [{ 'kind' => 'admin', 'score' => 1 }]
        }
      end

      let(:data_sparse) do
        {
          'name' => 'bob', 'age' => 41, 'tags' => %w[x],
          'scores' => [1, 'N/A'], 'users' => [{ 'kind' => 'ops' }]
        }
      end

      it 'errors caught at compile time always fail at runtime' do
        ['abs(email)', 'sum(tags)'].each do |expression|
          expect(check(expression).ok?).to be(false)
          [data_full, data_sparse].each do |data|
            expect { JMESPath.search(expression, data) }
              .to raise_error(Errors::InvalidTypeError)
          end
        end
      end

      it 'mixed arrays pass the check but may fail at runtime' do
        expect(check('sum(scores)').ok?).to be(true)
        expect(JMESPath.search('sum(scores)', data_full)).to eq(6)
        expect { JMESPath.search('sum(scores)', data_sparse) }
          .to raise_error(Errors::InvalidTypeError)
      end

      it 'optional fields pass the check but may fail at runtime' do
        expect(check('length(email)').ok?).to be(true)
        expect(JMESPath.search('length(email)', data_full)).to eq(5)
        expect { JMESPath.search('length(email)', data_sparse) }
          .to raise_error(Errors::InvalidTypeError)
      end
    end
  end
end
