# frozen_string_literal: true
require 'support'
require 'mustermann'
require 'mustermann/express'
require 'mustermann/pyramid'
require 'mustermann/template'
require 'mustermann/flask'

describe Mustermann::Pattern do
  describe :rename do
    it 'renames express captures without touching constraint text' do
      renamed = Mustermann.new('/:id(\d+id)', type: :express).rename(id: :file_id)
      renamed.to_s.should be == '/:file_id(\d+id)'
      renamed.should     be === '/42id'
      renamed.should_not be === '/42xx'
      renamed.params('/42id').should be == { 'file_id' => '42id' }
    end

    it 'renames pyramid captures without touching constraint text' do
      renamed = Mustermann.new('/{id:\d+}', type: :pyramid).rename(id: :file_id)
      renamed.to_s.should be == '/{file_id:\d+}'
      renamed.should     be === '/42'
      renamed.should_not be === '/abc'
      renamed.params('/42').should be == { 'file_id' => '42' }
    end

    it 'renames template variables' do
      renamed = Mustermann.new('/{id}', type: :template).rename(id: :file_id)
      renamed.to_s.should be == '/{file_id}'
      renamed.params('/42').should be == { 'file_id' => '42' }
      renamed.to_templates.should be == ['/{file_id}']
    end

    it 'renames exploded template variables' do
      renamed = Mustermann.new('{/segments*}', type: :template).rename(segments: :parts)
      renamed.to_s.should be == '{/parts*}'
      renamed.names.should be == ['parts']
    end

    it 'renames flask captures keeping converters intact' do
      renamed = Mustermann.new('/<int:id>', type: :flask).rename(id: :file_id)
      renamed.to_s.should be == '/<int:file_id>'
      renamed.params('/42').should be == { 'file_id' => 42 }
    end
  end
end
