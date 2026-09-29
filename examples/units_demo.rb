# Demonstrates Dentaku's optional unit-dimension mode:
#   1. mixed time units (seconds + milliseconds) with automatic conversion
#   2. derived dimensions (speed = length / time) and dimensionless ratios
#   3. a dimensionally incompatible addition, with the error pointing at
#      the exact subexpression that failed
#
# Run with: ruby -Ilib examples/units_demo.rb

require "dentaku"

calculator = Dentaku::Calculator.new(units: true)

puts "== 1. Mixed time units: seconds + milliseconds =="
calculator.declare_units(t1: :s, t2: :ms)
result = calculator.evaluate!("t1 + t2", t1: 2, t2: 500)
puts "t1 + t2            => #{result}"                # left operand's unit wins
puts "converted to ms    => #{result.to(:ms)}"
puts "converted to min   => #{result.to(:min)}"
puts

puts "== 2. Derived dimensions: speed and dimensionless ratios =="
quantity = Dentaku::Units.method(:quantity)
speed = calculator.evaluate!("distance / duration",
                             distance: quantity.call(120, :km), duration: quantity.call(2, :h))
puts "distance / duration => #{speed}"
puts "dimensions          => #{speed.unit.dimensions} (#{speed.unit.describe})"
puts "in base units       => #{speed.base_value.to_f.round(4)} m/s"

ratio = calculator.evaluate!("elapsed / total",
                             elapsed: quantity.call(500, :ms), total: quantity.call(2, :s))
puts "elapsed / total     => #{ratio} (#{ratio.class}: dimensions cancel out)"
puts

puts "== 3. Incompatible dimensions: length + time =="
begin
  calculator.evaluate!("1 + (length + duration) * 2",
                       length: quantity.call(10, :m), duration: quantity.call(5, :s))
rescue Dentaku::Units::MismatchError => e
  puts "raised Dentaku::Units::MismatchError:"
  puts "  #{e.message}"
end
puts

puts "== Plain formulas are unchanged =="
puts "1 + 2 * 3 => #{calculator.evaluate!('1 + 2 * 3')}"
