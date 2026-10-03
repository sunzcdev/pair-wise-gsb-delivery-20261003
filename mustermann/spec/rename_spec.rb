# frozen_string_literal: true
require 'support'
require 'mustermann'

describe Mustermann::Pattern do
  describe :rename do
    it 'raises a NotImplementedError when used directly' do
      expect { Mustermann::Pattern.new('').rename(a: :b) }.to raise_error(NotImplementedError)
    end

    describe 'with an AST-based pattern' do
      subject(:pattern) { Mustermann.new('/:id') }

      it 'returns a new pattern with the capture renamed' do
        renamed = pattern.rename(id: :account_id)
        renamed.should be_a Mustermann::Sinatra
        renamed.to_s.should be == '/:account_id'
        renamed.names.should be == ['account_id']
      end

      it 'leaves the original pattern unchanged' do
        pattern.rename(id: :account_id)
        pattern.to_s.should be == '/:id'
        pattern.names.should be == ['id']
      end

      it 'accepts a hash instead of keywords' do
        pattern.rename('id' => 'account_id').names.should be == ['account_id']
      end

      it 'returns itself if no renames are given' do
        pattern.rename.should be_equal pattern
      end

      it 'renames multiple captures at once' do
        renamed = Mustermann.new('/:a/:b').rename(a: :x, b: :y)
        renamed.to_s.should be == '/:x/:y'
        renamed.names.should be == %w[x y]
      end

      it 'renames captures in optional segments' do
        renamed = Mustermann.new('/:id(.:ext)?').rename(ext: :format)
        renamed.to_s.should be == '/:id(.:format)?'
        renamed.params('/42.png').should be == { 'id' => '42', 'format' => 'png' }
        renamed.params('/42').should be == { 'id' => '42', 'format' => nil }
      end

      it 'renames named splats' do
        renamed = Mustermann.new('/*file').rename(file: :document)
        renamed.to_s.should be == '/*document'
        renamed.names.should be == ['document']
        renamed.params('/a/b').should be == { 'document' => 'a/b' }
      end

      it 'renames captures in nested optional segments' do
        renamed = Mustermann.new('/:controller(/:action(/:id))', type: :rails).rename(action: :verb, id: :post_id)
        renamed.to_s.should be == '/:controller(/:verb(/:post_id))'
        renamed.params('/posts/show/1').should be == { 'controller' => 'posts', 'verb' => 'show', 'post_id' => '1' }
      end

      it 'makes names, params, expand and to_templates agree on the new names' do
        renamed = Mustermann.new('/:id(.:ext)?').rename(id: :file_id, ext: :format)
        renamed.names.should be == %w[file_id format]
        renamed.params('/42.png').should be == { 'file_id' => '42', 'format' => 'png' }
        renamed.expand(file_id: 42, format: 'png').to_s.should be == '/42.png'
        renamed.to_templates.should be == ['/{file_id}.{format}', '/{file_id}']
      end

      it 'carries capture constraints over to the new name' do
        renamed = Mustermann.new('/:id', capture: { id: /\d+/ }).rename(id: :file_id)
        renamed.should     be === '/42'
        renamed.should_not be === '/abc'
        renamed.params('/42').should be == { 'file_id' => '42' }
      end

      it 'does not rewrite literal text inside constraints' do
        renamed = Mustermann.new('/:id', capture: { id: /id\d+/ }).rename(id: :file_id)
        renamed.should     be === '/id5'
        renamed.should_not be === '/file5'
        renamed.params('/id5').should be == { 'file_id' => 'id5' }
      end

      it 'raises an ArgumentError for unknown capture names' do
        expect { pattern.rename(nope: :x) }.to raise_error(ArgumentError)
      end

      it 'raises an ArgumentError if the new name collides with an unrenamed capture' do
        expect { Mustermann.new('/:a/:b').rename(a: :b) }.to raise_error(ArgumentError)
      end

      it 'raises an ArgumentError if two captures are renamed to the same name' do
        expect { Mustermann.new('/:a/:b').rename(a: :x, b: :x) }.to raise_error(ArgumentError)
      end

      it 'raises an ArgumentError for invalid new names' do
        expect { pattern.rename(id: 'not a name') }.to raise_error(ArgumentError)
      end

      it 'raises an ArgumentError when renaming an anonymous splat' do
        expect { Mustermann.new('/*').rename(splat: :file) }.to raise_error(ArgumentError)
      end
    end

    describe 'with a composite pattern' do
      subject(:pattern) { Mustermann.new('/:a') | Mustermann.new(/\/(?<b>\w+)/) }

      it 'renames captures across sub-patterns' do
        renamed = pattern.rename(a: :x)
        renamed.should be_a Mustermann::Composite
        renamed.names.should be == %w[x b]
        renamed.params('/foo').should be == { 'x' => 'foo' }
      end

      it 'raises an ArgumentError if the rename collides with another sub-pattern' do
        expect { pattern.rename(a: :b) }.to raise_error(ArgumentError)
      end

      it 'raises an ArgumentError for unknown capture names' do
        expect { pattern.rename(nope: :x) }.to raise_error(ArgumentError)
      end
    end

    describe 'with a concatenated pattern' do
      subject(:pattern) { Mustermann.new('/:a') + Mustermann.new('/{b}', type: :template) }

      it 'renames captures across sub-patterns' do
        renamed = pattern.rename(a: :x, b: :y)
        renamed.should be_a Mustermann::Concat
        renamed.names.should be == %w[x y]
        renamed.params('/foo/bar').should be == { 'x' => 'foo', 'y' => 'bar' }
        renamed.expand(x: 1, y: 2).to_s.should be == '/1/2'
      end
    end

    describe 'reusing the same fragment with different names' do
      it 'allows concatenating two patterns derived from the same source' do
        fragment_a = Mustermann.new('/:id')
        fragment_b = Mustermann.new('/:id')

        combined = fragment_a.rename(id: :account_id) + fragment_b.rename(id: :file_id)
        combined.names.should be == %w[account_id file_id]
        combined.to_templates.should be == ['/{account_id}/{file_id}']

        url = combined.expand(account_id: 42, file_id: 7)
        url.to_s.should be == '/42/7'
        combined.params(url.to_s).should be == { 'account_id' => '42', 'file_id' => '7' }

        fragment_a.names.should be == ['id']
      end
    end
  end
end
