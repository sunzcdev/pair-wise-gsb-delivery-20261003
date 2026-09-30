$LOAD_PATH.unshift(File.expand_path('../lib', __dir__))
require 'dentaku'

class FakeSource
  def initialize(values, window_size: 4)
    @values = values
    @window_size = window_size
    @reads = []
  end

  attr_reader :reads

  def dentaku_stream?; true; end
  def dentaku_stream_window_size; @window_size; end
  def dentaku_stream_length; @values.length; end

  def dentaku_stream_read(range)
    @reads << range
    @values[range]
  end
end

c = Dentaku::Calculator.new
src = FakeSource.new((1..10).to_a)
c.store(samples: src)
c.store_formula('total', 'SUM(samples)')
c.store_formula('check', 'total > 50')
puts "formula deps: #{c.dependencies('total').inspect}"
puts "solved total: #{c.evaluate!('total')}"
puts "solved check: #{c.evaluate!('check')}"
puts "reads: #{src.reads.inspect}"

# direct AST node usage (no calculator session)
src2 = FakeSource.new((1..6).to_a, window_size: 2)
tokens = Dentaku::Tokenizer.new.tokenize('samples[5]')
node = Dentaku::Parser.new(tokens).parse
puts "direct AST: #{node.value('samples' => src2)}"
puts "direct reads: #{src2.reads.inspect}"

# dependencies/identifiers must not read
src3 = FakeSource.new((1..10).to_a)
c2 = Dentaku::Calculator.new
c2.identifiers('SUM(samples) + other')
c2.dependencies('SUM(samples)', samples: src3)
puts "no reads from identifiers/dependencies: #{src3.reads.inspect}"

# nested streaming aggregates: SUM of COUNT + ... ensure one stream per arg
begin
  c.evaluate!('SUM(samples) + SUM(samples)')
rescue => e
  puts "double aggregate: #{e.class}: #{e.message}"
end
puts "double aggregate reads (cached per eval): #{src.reads.inspect}"
