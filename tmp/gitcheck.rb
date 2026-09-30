begin
  out = `git ls-files`
  puts "exit=#{$?.exitstatus} lines=#{out.lines.size}"
rescue => e
  puts "ERR: #{e.class}: #{e.message}"
end
