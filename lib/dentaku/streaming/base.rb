require_relative '../streaming'

module Dentaku
  module Streaming
    # Optional convenience base class for streaming data sources. Subclasses
    # only need to implement #length and #read_window; the Dentaku protocol
    # methods are provided automatically.
    #
    #   class CsvSampleSource < Dentaku::Streaming::Base
    #     def initialize(path, window_size: 512)
    #       super(window_size: window_size, name: File.basename(path))
    #       @path = path
    #     end
    #
    #     def length
    #       @length ||= count_samples(@path)
    #     end
    #
    #     def read_window(range)
    #       CSV.foreach(@path).with_index.filter_map do |row, index|
    #         row.first.to_f if range.cover?(index)
    #       end
    #     end
    #   end
    class Base
      def initialize(window_size: nil, name: nil)
        @window_size = window_size
        @name = name
      end

      def dentaku_stream?
        true
      end

      def dentaku_stream_name
        @name || self.class.name
      end

      def dentaku_stream_window_size
        @window_size if @window_size
      end

      def dentaku_stream_length
        length
      end

      def dentaku_stream_read(range)
        read_window(range)
      end

      private

      def length
        raise NotImplementedError, "#{self.class} must implement #length"
      end

      def read_window(_range)
        raise NotImplementedError, "#{self.class} must implement #read_window"
      end
    end
  end
end
