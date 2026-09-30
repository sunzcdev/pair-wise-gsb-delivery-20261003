$LOAD_PATH.unshift(File.expand_path('../lib', __dir__))
require 'dentaku'

class FakeSource
  def initialize(values, window_size: 4, name: 'samples')
    @values = values
    @window_size = window_size
    @name = name
    @reads = []
  end

  attr_reader :reads

  def dentaku_stream?
    true
  end

  def dentaku_stream_name
    @name
  end

  def dentaku_stream_window_size
    @window_size
  end

  def dentaku_stream_length
    @values.length
  end

  def dentaku_stream_read(range)
    raise "full read forbidden" if range.begin.zero? && range.end >= @values.length - 1

    @reads << range
    @values[range]
  end
end

src = FakeSource.new([1, 2, 3, 4, 5, 6, 7, 8, 9, 10])
c = Dentaku::Calculator.new
c.store(samples: src)

puts "index:   #{c.evaluate!('samples[3]').inspect}"
puts "any:     #{c.evaluate!('ANY(samples, s, s > 8)').inspect}"
puts "all:     #{c.evaluate!('ALL(samples, s, s > 0)').inspect}"
puts "sum:     #{c.evaluate!('SUM(samples)').inspect}"
puts "avg:     #{c.evaluate!('AVG(samples)').inspect}"
puts "min:     #{c.evaluate!('MIN(samples)').inspect}"
puts "max:     #{c.evaluate!('MAX(samples)').inspect}"
puts "count:   #{c.evaluate!('COUNT(samples)').inspect}"
puts "reduce:  #{c.evaluate!('REDUCE(samples, m, s, m + s, 0)').inspect}"
puts "mul:     #{c.evaluate!('MUL(samples)').inspect}"
puts "reads:   #{src.reads.inspect}"

begin
  c.evaluate!('MAP(samples, s, s * 2)')
rescue Dentaku::ArgumentError => e
  puts "map rejected: #{e.message} (reason=#{e.reason})"
end
