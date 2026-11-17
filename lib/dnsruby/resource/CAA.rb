# --
# Copyright 2007 Nominet UK
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# ++
module Dnsruby
  class RR
    # Class for CAA resource records.
    # RFC 8659
    class CAA < RR
      ClassValue = nil #:nodoc: all
      TypeValue= Types::CAA #:nodoc: all

      # The property tag. Tags are extensible ASCII letters and digits.
      attr_accessor :property_tag
      # The value for the property_tag
      attr_accessor :property_value
      # The value for the flag
      def flag=(value)
        @flag = value.to_i
      end

      def from_hash(hash) #:nodoc: all
        from_data([hash[:flag], hash[:property_tag], hash[:property_value]])
      end

      def from_data(data) #:nodoc: all
        flag, @property_tag, @property_value = data
        self.flag = flag
        validate_fields
      end

      def flag
        @flag.to_i
      end

      def from_string(input) #:nodoc: all
        # A CAA value is one character-string, with no 255-octet limit.
        matches = /\A\s*(\d+)\s+([A-Za-z0-9]+)\s+("(?:\\.|[^"\\])*"|(?:\\.|[^\s"\\])+)\s*\z/m.match(input.b)
        raise DecodeError, "Cannot parse CAA record: #{input[0...1000]}" unless matches

        value = matches[3]
        value = value[1...-1] if value.start_with?('"')
        from_data([matches[1], matches[2], decode_value(value)])
      end

      def rdata_to_string #:nodoc: all
        value = +''.b
        @property_value.each_byte do |octet|
          if octet == 34 || octet == 92
            value << 92 << octet
          elsif octet < 32 || octet > 126
            value << format('\\%03d', octet)
          else
            value << octet
          end
        end
        "#{flag} #{@property_tag} \"#{value}\""
      end

      def encode_rdata(msg, canonical=false) #:nodoc: all
        validate_fields
        msg.put_pack('C', flag)
        msg.put_string(@property_tag)
        # We don't put a length byte on the final string.
        msg.put_bytes(@property_value)
      end

      def self.decode_rdata(msg) #:nodoc: all
        flag, = msg.get_unpack('C')
        property_tag = msg.get_string
        # The final string has no length byte - its length is implicit as the remainder of the packet length
        property_value = msg.get_bytes
        record = self.new([flag, property_tag, property_value])
        # Keep the public construction-time rdata String used by Comparable
        # and older serializers, without reparsing arbitrary wire value bytes.
        record.rdata = record.rdata_to_string
        record
      end

      def validate_fields
        unless (0..255).cover?(flag) && @property_tag.is_a?(String) &&
            @property_tag.bytesize.between?(1, 255) && /\A[A-Za-z0-9]+\z/n.match?(@property_tag.b) &&
            @property_value.is_a?(String)
          raise DecodeError, 'CAA requires an octet flag, an alphanumeric tag and a string value'
        end
      end
      private :validate_fields

      def decode_value(value)
        decoded = +''.b
        index = 0
        while index < value.bytesize
          octet = value.getbyte(index)
          if octet == 92
            index += 1
            octet = value.getbyte(index)
            raise DecodeError, 'Incomplete CAA value escape' unless octet
            if (48..57).cover?(octet)
              digits = value.byteslice(index, 3)
              unless /\A[0-9]{3}\z/.match?(digits) && digits.to_i <= 255
                raise DecodeError, 'CAA decimal escapes must specify one octet with three digits'
              end
              octet = digits.to_i
              index += 2
            end
          end
          decoded << octet
          index += 1
        end
        decoded
      end
      private :decode_value
    end
  end
end
