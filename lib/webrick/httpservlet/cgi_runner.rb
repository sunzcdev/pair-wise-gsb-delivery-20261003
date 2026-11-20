# frozen_string_literal: true
#
# cgi_runner.rb -- CGI launcher.
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2000 TAKAHASHI Masayoshi, GOTOU YUUZOU
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: cgi_runner.rb,v 1.9 2002/09/25 11:33:15 gotoyuzo Exp $

def sysread(io, size)
  buf = +""
  while size > 0
    tmp = io.sysread(size)
    buf << tmp
    size -= tmp.bytesize
  end
  return buf
end

STDIN.binmode
STDOUT.binmode
STDERR.binmode

# The CGIHandler spawns us with three pipes attached to STDIN, STDOUT and
# STDERR.  STDIN first carries the marshaled CGI environment; everything left
# on it afterwards is the request body, which the executed script inherits
# unchanged through the exec below.
len  = sysread(STDIN, 8).to_i
dump = sysread(STDIN, len)
hash = Marshal.restore(dump)
ENV.keys.each{|name| ENV.delete(name) }
hash.each{|k, v| ENV[k] = v if v }

dir = File::dirname(ENV["SCRIPT_FILENAME"])
Dir::chdir dir

if ARGV[0]
  argv = ARGV.dup
  argv << ENV["SCRIPT_FILENAME"]
  exec(*argv)
  # NOTREACHED
end
exec ENV["SCRIPT_FILENAME"]
