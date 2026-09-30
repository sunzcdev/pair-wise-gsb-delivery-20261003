# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe StaticTypeChecker do
    let(:shape) do
      Shape.object(
        id: Shape.number,
        name: Shape.string,
        active: Shape.boolean,
        age: Shape.optional(Shape.number),
        score: Shape.union(Shape.number, Shape.null),
        tags: Shape.array(Shape.string),
        numbers: Shape.array(Shape.number),
        mixed: Shape.array(Shape.union(Shape.number, Shape.string)),
        people: Shape.array(Shape.object(
          kind: Shape.string,
          years: Shape.optional(Shape.number)
        )),
        nested: Shape.object(
          child: Shape.object(value: Shape.number)
        )
      )
    end

    def check(expression, input = shape)
      JMESPath.check(expression, input)
    end

    describe 'output shape inference' do
      it 'infers a required field' do
        expect(check('id').output_shape).to eq(Shape.number)
      end

      it 'infers an optional field as type | null' do
        expect(check('age').output_shape).to eq(Shape.union(Shape.number, Shape.null))
      end

      it 'infers a nullable union field' do
        expect(check('score').output_shape.to_s).to eq('number | null')
      end

      it 'infers array element types through a wildcard' do
        expect(check('tags[*]').output_shape.to_s).to eq('array<string>')
      end

      it 'infers nested object and array combinations' do
        expect(check('people[*].years').output_shape.to_s).to eq('array<number>')
      end

      it 'infers multi-select hashes with required keys' do
        output = check('{i: id, a: age}').output_shape
        expect(output.to_s).to eq('{a:number | null, i:number}')
      end

      it 'infers multi-select lists as tuples unioned into an array' do
        expect(check('[id, name]').output_shape.to_s).to eq('array<number | string>')
      end

      it 'infers array index access as element | null' do
        expect(check('numbers[0]').output_shape.to_s).to eq('number | null')
      end

      it 'infers literals' do
        expect(check('`true`').output_shape).to eq(Shape.boolean)
        expect(check('`"x"`').output_shape).to eq(Shape.string)
      end

      it 'infers flatten results' do
        nested = Shape.object(matrix: Shape.array(Shape.array(Shape.number)))
        expect(check('matrix[]', nested).output_shape.to_s).to eq('array<number>')
        expect(check('matrix[] | [0]', nested).output_shape.to_s).to eq('number | null')
      end

      it 'infers function return types' do
        expect(check('length(tags)').output_shape).to eq(Shape.number)
        expect(check('keys(nested)').output_shape.to_s).to eq('array<string> | null')
      end

      it 'infers or/and unions' do
        expect(check('age || id').output_shape.nullable?).to be(true)
      end
    end

    describe 'definite compile-time errors' do
      it 'flags abs() on a definite string' do
        result = check('abs(name)')
        expect(result).not_to be_ok
        expect(result.errors.first.message).to include('abs()')
      end

      it 'flags abs() on a definite object' do
        expect(check('abs(nested)')).not_to be_ok
      end

      it 'flags abs() on a definite array even with unknown elements' do
        input = Shape.object(things: Shape.array(Shape.any))
        expect(check('abs(things)', input)).not_to be_ok
      end

      it 'flags arity mismatches' do
        result = check('abs(`1`, `2`)')
        expect(result.errors.first.message).to include('2 argument')
      end

      it 'flags starts_with on a number' do
        expect(check('starts_with(id, `x`)')).not_to be_ok
      end

      it 'flags avg/sum over a definite string array' do
        expect(check('sum(tags)')).not_to be_ok
        expect(check('avg(tags)')).not_to be_ok
      end

      it 'flags sort over a definite object array' do
        expect(check('sort(people)')).not_to be_ok
      end

      it 'flags join with an array of numbers' do
        expect(check('join(`,`, numbers)')).not_to be_ok
      end

      it 'flags length on a definite number' do
        expect(check('length(id)')).not_to be_ok
      end

      it 'flags keys() on an array' do
        expect(check('keys(tags)')).not_to be_ok
      end

      it 'flags map() without an expression reference' do
        expect(check('map(id, numbers)')).not_to be_ok
      end

      it 'flags sort_by() with a non-array first argument' do
        expect(check('sort_by(id, &value)')).not_to be_ok
      end

      it 'attaches the node and derivation reasons' do
        finding = check('abs(name)').errors.first
        expect(finding.node_class).to eq(Nodes::AbsFunction)
        expect(finding.reasons).to include(a_string_including('argument is'))
        expect(finding.trail).to be_an(Array)
      end
    end

    describe 'deferred runtime checks' do
      it 'does not flag abs() on an optional number' do
        result = check('abs(age)')
        expect(result).to be_ok
        expect(result.runtime_checks.size).to eq(1)
      end

      it 'does not flag abs() over unknown data' do
        result = check('abs(a.b.c)', Shape.any)
        expect(result).to be_ok
        expect(result.runtime_checks).not_to be_empty
      end

      it 'does not flag avg() on a mixed number|string array' do
        result = check('avg(mixed)')
        expect(result).to be_ok
        expect(result.runtime_checks.map(&:message).join).to include('avg()')
      end

      it 'does not flag a field missing from an open object' do
        result = check('unknown_field')
        expect(result).to be_ok
        expect(result.runtime_checks.first.message).to include('not declared')
      end

      it 'does not flag valid length/join calls' do
        expect(check('length(tags)')).to be_ok
        expect(check('join(`,`, tags)')).to be_ok
      end

      it 'treats an absent key on a closed object as null, not an error' do
        closed = Shape.object({ a: Shape.number }, open: false)
        result = check('b', closed)
        expect(result).to be_ok
        expect(result.output_shape).to eq(Shape.null)
      end
    end

    describe 'filter narrowing' do
      it 'removes null inside an equality-filtered branch' do
        expect(check('people[?years != `null`].years').output_shape.to_s)
          .to eq('array<number>')
      end

      it 'keeps null without a narrowing filter' do
        expect(check('people[*].years').output_shape.to_s).to eq('array<number>')
      end

      it 'narrows ordering comparisons to number' do
        expect(check('people[?years > `0`].years').output_shape.to_s)
          .to eq('array<number>')
      end

      it 'accepts abs() after narrowing but not without it' do
        narrowed = check('abs(people[?years != `null`] | [0].years)')
        expect(narrowed).to be_ok

        unguarded = check('abs(people[0].years)')
        expect(unguarded).to be_ok
        expect(unguarded.runtime_checks.map(&:message).join).to include('abs()')
      end

      it 'narrows nullable union fields' do
        array_root = Shape.array(Shape.object(
          score: Shape.union(Shape.number, Shape.null)
        ))
        expect(JMESPath.check('[?score != `null`].score', array_root).output_shape.to_s)
          .to eq('array<number>')
      end

      it 'propagates narrowing into map expressions' do
        array_root = Shape.array(Shape.object(
          age: Shape.optional(Shape.number)
        ))
        root = Shape.object(team: array_root)
        expect(check('map(&age, team[?age != `null`])', root).output_shape.to_s)
          .to eq('array<number>')
        expect(check('map(&age, team)', root).output_shape.to_s)
          .to eq('array<number | null>')
      end
    end

    describe 'object wildcard and closed shapes' do
      it 'projects closed object values precisely' do
        closed = Shape.object({ a: Shape.number, b: Shape.string }, open: false)
        expect(check('*', closed).output_shape.to_s).to eq('array<number | string>')
      end

      it 'keeps open object wildcard as any' do
        open_shape = Shape.object(a: Shape.number)
        expect(check('*', open_shape).output_shape.to_s).to eq('array<any>')
      end

      it 'infers values() over a closed object' do
        closed = Shape.object({ a: Shape.number }, open: false)
        expect(check('values(@)', closed).output_shape.to_s).to eq('array<number> | null')
      end
    end

    describe '.check!' do
      it 'returns the result when clean' do
        expect(JMESPath.check!('id', shape).output_shape).to eq(Shape.number)
      end

      it 'raises StaticTypeError on definite errors' do
        expect { JMESPath.check!('abs(name)', shape) }
          .to raise_error(Errors::StaticTypeError)
      end
    end

    describe 'Shape.from_value / from_values' do
      it 'infers from a concrete hash' do
        shape = Shape.from_value({ 'a' => 1, 'b' => 'x' })
        expect(shape.lookup('a')).to eq(Shape.number)
        expect(shape.lookup('b')).to eq(Shape.string)
      end

      it 'marks requested keys optional' do
        shape = Shape.from_value({ 'a' => 1, 'b' => 2 }, ['b'])
        expect(shape.lookup('b').nullable?).to be(true)
        expect(shape.lookup('a').nullable?).to be(false)
      end

      it 'unions heterogeneous examples and arrays' do
        shape = Shape.from_values({ 'a' => 1 }, { 'a' => 'x' })
        expect(shape.to_s).to include('number').and include('string')
      end
    end
  end
end
