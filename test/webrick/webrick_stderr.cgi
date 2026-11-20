#!ruby
# A CGI that writes a large amount of stderr while still producing a normal
# response.  stderr must be drained independently so it cannot block the
# business response or corrupt its headers.
STDIN.binmode
STDOUT.binmode
STDERR.binmode

# Drain the (possibly empty) request body.
content_length = (ENV["CONTENT_LENGTH"] || 0).to_i
read = 0
while read < content_length
  chunk = STDIN.read([content_length - read, 65536].min)
  break if chunk.nil? || chunk.empty?
  read += chunk.bytesize
end

# More than a pipe buffer worth of diagnostics.
STDERR.write("e" * (2 * 1024 * 1024))
STDERR.flush

STDOUT.write("content-type: text/plain\r\n\r\n")
STDOUT.write("stderr-done")
