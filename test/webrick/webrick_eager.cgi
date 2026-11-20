#!ruby
# A CGI that emits a large response before it finishes reading the request
# body.  A strictly sequential pipe implementation can deadlock here once
# the pipe buffers fill up; the concurrent pump threads must keep both the
# request body and the response flowing at the same time.
#
# cgi_runner.rb has already consumed the framed environment; STDIN now holds
# only the raw request body and the CGI environment lives in ENV.
STDIN.binmode
STDOUT.binmode
STDERR.binmode

content_length = (ENV["CONTENT_LENGTH"] || 0).to_i

# Emit far more output than any pipe buffer can hold, before consuming a
# single byte of the request body.
STDOUT.write("content-type: text/plain\r\n\r\n")
STDOUT.write("x" * (2 * 1024 * 1024))
STDOUT.flush

# Now drain the request body and echo it back so the test can verify that
# the full body survived the round trip.
body = +""
while body.bytesize < content_length
  chunk = STDIN.read([content_length - body.bytesize, 65536].min)
  break if chunk.nil? || chunk.empty?
  body << chunk
end
STDOUT.write(body)
