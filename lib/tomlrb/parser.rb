# frozen-string-literal: true

require 'tomlrb/generated_parser'

class Tomlrb::Parser < Tomlrb::GeneratedParser

  def initialize(tokenizer, handler: nil, **options)
    @tokenizer = tokenizer
    @handler   = handler || Tomlrb::Handler.new(**options)
    super()
  end

  def next_token
    @tokenizer.next_token
  end

  def parse
    do_parse
    @handler
  end
end
