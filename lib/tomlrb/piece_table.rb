# frozen_string_literal: true

module Tomlrb
  # A piece table that keeps the original document immutable.  Each piece is
  # either +[:orig, [from, to]]+ (a half-open byte range of the original
  # text) or +[:new, string]+ (synthesised content).  This lets {Document}
  # prove which original bytes survived an edit session.
  class PieceTable
    attr_reader :original

    def initialize(original)
      @original = original
      @pieces = [[:orig, [0, original.bytesize]]]
    end

    def to_s
      buffer = String.new(encoding: Encoding::UTF_8)
      @pieces.each do |kind, payload|
        buffer << piece_text(kind, payload)
      end
      buffer
    end

    # Applies non-overlapping edits [from, to, replacement] in current-text
    # byte coordinates.
    def apply(edits)
      raise ArgumentError, 'No edits given' if edits.empty?

      ordered = edits.sort_by { |from, to, _| [from, to] }
      ordered.each_cons(2) do |edit_a, edit_b|
        raise ArgumentError, 'Overlapping edits' if edit_a[1] > edit_b[0]
      end
      ordered.each do |from, to, _|
        raise ArgumentError, 'Backwards edit' if to < from
        raise ArgumentError, 'Edit outside document' if to > length
      end

      # Apply edits from the end so offsets of earlier edits stay valid.
      pieces = @pieces.map(&:dup)
      ordered.sort_by(&:first).reverse.each do |from, to, replacement|
        pieces = apply_one(pieces, from, to, replacement)
      end

      @pieces = pieces
      self
    end

    def length
      @pieces.sum { |kind, payload| piece_length(kind, payload) }
    end

    # [current_from, current_to, original_from] for surviving original bytes.
    def surviving_original_ranges
      ranges = []
      position = 0
      @pieces.each do |kind, payload|
        piece_len = piece_length(kind, payload)
        if kind == :orig
          ranges << [position, position + piece_len, payload[0]]
        end
        position += piece_len
      end
      ranges
    end

    private

    def piece_text(kind, payload)
      kind == :orig ? @original.byteslice(payload[0], payload[1] - payload[0]) : payload
    end

    def piece_length(kind, payload)
      kind == :orig ? payload[1] - payload[0] : payload.bytesize
    end

    def offsets_of(pieces)
      offsets = []
      position = 0
      pieces.each do |kind, payload|
        offsets << position
        position += piece_length(kind, payload)
      end
      offsets
    end

    # Returns the piece index containing +offset+.  With
    # +insert_boundary: true+, an offset sitting exactly on a piece start is
    # reported as that piece; otherwise an offset at a piece start is the end
    # of the preceding piece.
    def apply_one(pieces, from, to, replacement)
      offsets = offsets_of(pieces)
      start_index, start_local = locate(pieces, offsets, from)

      if to == from
        insert_at(pieces, start_index, start_local, from, replacement)
      else
        end_index, end_local = locate(pieces, offsets, to - 1)
        end_local += 1 # exclusive within the end piece
        replace_span(pieces, start_index, start_local, end_index, end_local, replacement)
      end
    end

    def locate(pieces, offsets, offset)
      index = offsets.rindex { |start| start <= offset }
      raise ArgumentError, "Position #{offset} out of range" if index.nil?
      [index, offset - offsets[index]]
    end

    def insert_at(pieces, index, local, offset, replacement)
      return pieces if replacement.empty?
      kind, payload = pieces[index]
      if local.zero?
        pieces.insert(index, [:new, replacement])
      elsif local == piece_length(kind, payload)
        pieces.insert(index + 1, [:new, replacement])
      else
        left, right = split_piece(kind, payload, local)
        pieces[index, 1] = [left, [:new, replacement], right]
      end
      pieces
    end

    def replace_span(pieces, start_index, start_local, end_index, end_local, replacement)
      start_kind, start_payload = pieces[start_index]
      end_kind, end_payload = pieces[end_index]
      prefix = start_local.zero? ? nil : slice_piece(start_kind, start_payload, 0, start_local)
      suffix =
        if end_local == piece_length(end_kind, end_payload)
          nil
        else
          slice_piece(end_kind, end_payload, end_local, piece_length(end_kind, end_payload) - end_local)
        end

      replacement_pieces = []
      replacement_pieces << prefix if prefix
      replacement_pieces << [:new, replacement] unless replacement.empty?
      replacement_pieces << suffix if suffix
      pieces[start_index..end_index] = replacement_pieces
      pieces
    end

    def split_piece(kind, payload, local)
      if kind == :orig
        [[:orig, [payload[0], payload[0] + local]],
         [:orig, [payload[0] + local, payload[1]]]]
      else
        [[:new, payload.byteslice(0, local)],
         [:new, payload.byteslice(local, payload.bytesize - local)]]
      end
    end

    def slice_piece(kind, payload, from, length)
      if kind == :orig
        [:orig, [payload[0] + from, payload[0] + from + length]]
      else
        [:new, payload.byteslice(from, length)]
      end
    end
  end
end
