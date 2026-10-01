# frozen-string-literal: true

require 'tomlrb/generated_parser'

class Tomlrb::Parser < Tomlrb::GeneratedParser

  def initialize(tokenizer, **options)
    @tokenizer = tokenizer
    @handler   = Tomlrb::Handler.new(**options)
    super()
  end

  def next_token
    @tokenizer.next_token
  end

  def key_part(value)
    value = value[0] if value.is_a?(Array)
    Tomlrb::KeyPart.new(value.to_s, @tokenizer.location)
  end

  def split_key_parts(text)
    location = @tokenizer.location
    offset = 0
    text.split('.', -1).map do |segment|
      part = Tomlrb::KeyPart.new(segment, Tomlrb::Location.new(location.line, location.column + offset))
      offset += segment.length + 1
      part
    end
  end

  def parse
    do_parse
    @handler
  end
end
