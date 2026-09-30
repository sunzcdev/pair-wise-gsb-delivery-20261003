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
  def dentaku_stream_name; 'samples'; end
  def dentaku_stream_window_size; @window_size; end
  def dentaku_stream_length; @values.length; end

  def dentaku_stream_read(range)
    raise 'full read forbidden' if range.begin.zero? && range.end >= @values.length - 1

    @reads << range
    @values[range]
  end
end

# identifier cache enabled
Dentaku.enable_identifier_cache!
src = FakeSource.new((1..10).to_a)
c = Dentaku::Calculator.new
puts "cache-on SUM: #{c.evaluate!('SUM(samples)', samples: src)}"
puts "reads: #{src.reads.inspect}"
src2 = FakeSource.new((1..10).to_a)
puts "cache-on index twice: #{c.evaluate!('samples[2] + samples[3]', samples: src2)}"
puts "reads: #{src2.reads.inspect}"

# bulk solve
src3 = FakeSource.new((1..5).to_a)
c2 = Dentaku::Calculator.new
c2.store(samples: src3)
result = c2.solve!({ total: 'SUM(samples)', big: 'ANY(samples, s, s > 3)' })
puts "solve!: #{result.inspect}"
puts "solve! reads: #{src3.reads.inspect}"

# memory leak check
c3 = Dentaku::Calculator.new
c3.evaluate!('1 + 1')
puts "memory keys after eval: #{c3.memory.keys.inspect}"

# permissive read failure
class FailSource < FakeSource
  def dentaku_stream_read(range)
    raise 'disk on fire' if range.begin.zero?

    super
  end
end
puts "permissive failure: #{c2.evaluate('SUM(s)', s: FailSource.new((1..10).to_a)).inspect}"

# plain arrays unaffected end to end
puts "array MAP: #{Dentaku('MAP(xs, x, x + 1)', xs: [1, 2, 3]).inspect}"
puts "array SUM: #{Dentaku('SUM(xs)', xs: [1, 2, 3]).inspect}"
