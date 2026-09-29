# Demonstrates Dentaku's optional unit dimension mode:
#   1. mixing seconds and milliseconds (automatic conversion)
#   2. computing speed (derived dimension m/s)
#   3. adding length to time (error located at the subexpression)
#
# Run with: ruby -Ilib examples/units_demo.rb

require 'dentaku'

calculator = Dentaku::Calculator.new(units: true)

puts '1. Mixing seconds and milliseconds'
total = calculator.evaluate!('response_time + queue_time',
  response_time: { value: 90, unit: :s },
  queue_time:    { value: 1500, unit: :ms })
puts "   response_time + queue_time = #{total}" # 91.5 s
puts "   (dimension: #{Dentaku::Units.describe(total.dimension)})"
puts

puts '2. Speed from distance and time'
speed = calculator.evaluate!('distance / time',
  distance: { value: 100, unit: :m },
  time:     { value: 9.58, unit: :s })
puts "   distance / time = #{speed.round(2)}" # 10.44 m/s
puts "   (dimension: #{Dentaku::Units.describe(speed.dimension)})"

ratio = calculator.evaluate!('completed / total',
  completed: { value: 75, unit: :m },
  total:     { value: 100, unit: :m })
puts "   completed / total = #{ratio.to_f} (dimensionless ratio, plain numeric)"
puts

puts '3. Length plus time raises a located error'
begin
  calculator.evaluate!('(track_length + lap_time) * 2',
    track_length: { value: 400, unit: :m },
    lap_time:     { value: 63, unit: :s })
rescue Dentaku::UnitError => e
  puts "   #{e.message}"
  puts "   offending subexpression: #{e.expression}"
end
