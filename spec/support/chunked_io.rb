class ChunkedIO
  attr_reader :call_count, :max_request, :max_returned, :total_returned

  def initialize(string, chunk_size = 3)
    @string = string
    @chunk_size = chunk_size
    @offset = 0
    @call_count = 0
    @max_request = 0
    @max_returned = 0
    @total_returned = 0
  end

  def read(length = nil, outbuf = nil)
    @call_count += 1
    @max_request = length if length && length > @max_request
    limit = length ? [length, @chunk_size].min : @chunk_size
    piece = @string.byteslice(@offset, limit)
    @offset += piece.bytesize if piece
    piece = +"" if piece.nil?
    @max_returned = [@max_returned, piece.bytesize].max
    @total_returned += piece.bytesize
    outbuf ? outbuf.replace(piece) : piece
  end

  def eof?
    @offset >= @string.bytesize
  end
end

class GuardedIO < ChunkedIO
  def initialize(string, chunk_size, max_length: chunk_size, forbid_full: true)
    super(string, chunk_size)
    @max_length = max_length
    @forbid_full = forbid_full
  end

  def read(length = nil, outbuf = nil)
    raise "document slurped in a single read" if @forbid_full && length && length >= @string.bytesize
    raise "oversized read request: #{length}" if length && length > @max_length
    super(length, outbuf)
  end
end
