# frozen_string_literal: true
require 'support'
require 'mustermann'

describe 'Pattern#rename' do
  context 'ordinary captures' do
    subject(:pattern) { Mustermann.new('/:id') }
    let(:renamed)     { pattern.rename(id: :account_id) }

    it('returns a new pattern')                    { expect(renamed).not_to be(pattern)          }
    it('exposes the new name')                     { expect(renamed.names).to be == %w[account_id] }
    it('matches with the new name')                { expect(renamed.params('/42')).to be == { 'account_id' => '42' } }
    it('expands with the new name')                { expect(renamed.expand(account_id: 42)).to be == '/42' }
    it('exports templates with the new name')      { expect(renamed.to_templates).to be == ['/{account_id}'] }

    it('leaves the original names alone')          { expect(pattern.names).to be == %w[id] }
    it('leaves the original params alone')         { expect(pattern.params('/42')).to be == { 'id' => '42' } }
    it('leaves the original expansion alone')      { expect(pattern.expand(id: 42)).to be == '/42' }
    it('leaves the original templates alone')      { expect(pattern.to_templates).to be == ['/{id}'] }

    it 'accepts string keys and values' do
      expect(pattern.rename('id' => 'account_id').names).to be == %w[account_id]
    end

    it 'returns itself if the mapping is empty' do
      expect(pattern.rename).to be(pattern)
    end

    it 'returns itself if no known capture is mapped' do
      expect(pattern.rename(foo: :bar)).to be(pattern)
    end
  end

  context 'named splats' do
    subject(:renamed) { Mustermann.new('/files/*path').rename(path: :file_path) }

    it('exposes the new name')           { expect(renamed.names).to be == %w[file_path] }
    it('matches with the new name')      { expect(renamed.params('/files/a/b')).to be == { 'file_path' => 'a/b' } }
    it('expands with the new name')      { expect(renamed.expand(file_path: 'a/b')).to be == '/files/a/b' }
    it('exports templates with new name') { expect(renamed.to_templates).to be == ['/files/{+file_path}'] }
  end

  context 'unnamed splats' do
    subject(:renamed) { Mustermann.new('/*').rename(splat: :path) }

    it('turns into a named splat')  { expect(renamed.names).to be == %w[path] }
    it('matches with the new name') { expect(renamed.params('/a/b')).to be == { 'path' => 'a/b' } }
  end

  context 'optional segments' do
    subject(:renamed) { Mustermann.new('/:id(.:ext)?').rename(id: :account_id, ext: :format) }

    it('exposes the new names') { expect(renamed.names).to be == %w[account_id format] }

    it 'matches with the new names' do
      expect(renamed.params('/42.json')).to be == { 'account_id' => '42', 'format' => 'json' }
      expect(renamed.params('/42')).to be == { 'account_id' => '42', 'format' => nil }
    end

    it 'expands with the new names' do
      expect(renamed.expand(account_id: 42)).to be == '/42'
      expect(renamed.expand(account_id: 42, format: 'json')).to be == '/42.json'
    end

    it('exports templates with the new names') do
      expect(renamed.to_templates).to be == ['/{account_id}.{format}', '/{account_id}']
    end
  end

  context 'constraints' do
    it 'keeps constraints working under the new name' do
      renamed = Mustermann.new('/:id', capture: { id: /\d+/ }).rename(id: :account_id)
      expect(renamed.params('/42')).to be == { 'account_id' => '42' }
      expect(renamed.params('/abc')).to be_nil
    end

    it 'does not rewrite literal constraint text' do
      renamed = Mustermann.new('/:id', capture: { id: 'id' }).rename(id: :account_id)
      expect(renamed.params('/id')).to be == { 'account_id' => 'id' }
      expect(renamed.params('/account_id')).to be_nil
    end
  end

  context 'invalid rebindings' do
    it 'raises if the new name collides with an unrenamed capture' do
      expect { Mustermann.new('/:a/:b').rename(a: :b) }.to raise_error(Mustermann::Error)
    end

    it 'raises if two captures are mapped to the same name' do
      expect { Mustermann.new('/:a/:b').rename(a: :c, b: :c) }.to raise_error(Mustermann::Error)
    end

    it 'raises if the new name is not a valid capture name' do
      expect { Mustermann.new('/:a').rename(a: '1bad') }.to raise_error(Mustermann::Error)
    end
  end

  context 'composite patterns' do
    subject(:renamed) { (Mustermann.new('/:id') | Mustermann.new('/:id/:name')).rename(id: :account_id) }

    it('exposes the new name')      { expect(renamed.names).to be == %w[account_id name] }
    it('matches with the new name') { expect(renamed.params('/42')).to be == { 'account_id' => '42' } }

    it 'renames concatenated patterns' do
      concat   = Mustermann.new('/:a', uri_decode: false) + Mustermann.new('/:b')
      renamed  = concat.rename(a: :x, b: :y)
      expect(renamed.names).to be == %w[x y]
      expect(renamed.params('/1/2')).to be == { 'x' => '1', 'y' => '2' }
    end
  end

  context 'reusing a fragment with different bindings' do
    let(:fragment) { proc { Mustermann.new('/:id') } }
    let(:pattern)  { fragment.call.rename(id: :account_id) + fragment.call.rename(id: :file_id) }

    it('exposes both names')        { expect(pattern.names).to be == %w[account_id file_id] }
    it('expands to a URL')          { expect(pattern.expand(account_id: 42, file_id: 7)).to be == '/42/7' }
    it('exports templates')         { expect(pattern.to_templates).to be == ['/{account_id}/{file_id}'] }

    it 'takes both values back from the generated URL' do
      url = pattern.expand(account_id: 42, file_id: 7)
      expect(pattern.params(url)).to be == { 'account_id' => '42', 'file_id' => '7' }
    end
  end

  context 'unsupported patterns' do
    it 'is a no-op for patterns without captures' do
      pattern = Mustermann.new('/foo', type: :identity)
      expect(pattern.rename(id: :account_id)).to be(pattern)
    end

    it 'raises a NotImplementedError for patterns that cannot rename captures' do
      pattern = Mustermann.new(/(?<id>\d+)/, type: :regexp)
      expect { pattern.rename(id: :account_id) }.to raise_error(NotImplementedError)
    end
  end
end
