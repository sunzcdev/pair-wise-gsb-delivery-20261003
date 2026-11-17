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
begin
  require 'jcode'
rescue LoadError => _e
end
module Dnsruby
  class RR
    # Class for DNS Text (TXT) resource records.
    # RFC 1035 Section 3.3.14
    class TXT < RR
      ClassValue = nil #:nodoc: all
      TypeValue = Types::TXT #:nodoc: all

      # List of the individual elements
      attr_accessor :strings

      def data
        @strings.join
      end

      def from_data(data)
        @strings = data
      end

      def from_hash(hash)
        if (hash.has_key?:strings)
          from_string(hash[:strings])
        end
      end

      ESCAPE_CHARS = {"b" => 8, "t" => 9, "n" => 10, "v" => 11, "f" => 12, "r" => 13}
      ESCAPE_CODES = ESCAPE_CHARS.invert

      def from_string(input)
        @strings = TXT.parse(input)
      end

      def TXT.parse(input)
        #  Need to look out for special characters.
        #  Need to split the input up into strings (which are defined by non-escaped " characters)
        #  Then need to fix up any \ escape characters (should just be " and ; and binary?)
        #  Sadly, it's going to be easiest just to scan through this character by character...
        in_escaped = false
        in_string = false
        count = -1
        strings = []
        input_encoding = input.encoding
        finish_strings = lambda do
          strings.each do |string|
            string.force_encoding(input_encoding)
            string.force_encoding(Encoding::BINARY) unless string.valid_encoding?
          end
        end
        current_binary = +""
        current_quote_char = '"'
        unquoted = false
        seen_strings = false
        pos = 0
        input = input.b.sub(/^\s*\(\s*/, "")
        input.sub!(/\s*\)\s*$/, "")
        input.each_char {|c|
          if (((c == "'") || (c == '"')) && (!in_escaped) && (!unquoted))
            if (!in_string)
              seen_strings = true
              current_quote_char = c
              in_string = true
              count+=1
              strings[count] = "".b
            else
              if (c == current_quote_char)
                in_string = false
              else
                strings[count] << c
              end
            end
          else
            if (seen_strings && !in_string)
              if (c == ";")
                 # Comment in zone file!
                 return finish_strings.call
              end
              if (c != " " && c != "\t")
                in_string = true
                count+=1
                strings[count] = "".b
              else
                next
              end
            end
            if (pos == 0)
              unquoted = true
              count+=1
              strings[count] = "".b
            elsif (unquoted)
              if (c == " ")
                count+=1
                strings[count] = "".b
                pos += 1
                next
              end
            end

            if (c == "\\")
              if (in_escaped)
                in_escaped = false
                strings[count] << c
              else
                in_escaped = true
              end
            else
              if (in_escaped)
                #  Build up the binary
                if (c == ";") || (c == '"')
                  strings[count] << c
                  in_escaped = false
                elsif (ESCAPE_CHARS[c])
                  in_escaped=false
                  strings[count] << ESCAPE_CHARS[c].chr
                elsif (c<"0" || c>"9")
                  in_escaped = false
                  strings[count] << c
                else
                  #  Must be building up three digit string to identify binary value?
#                  if (c >= "0" && c <= "9")
                    current_binary << c
#                  end
                  if ((current_binary.length == 3) ) # || (c < "0" || c > "9"))
                    strings[count] << current_binary.to_i.chr
                    in_escaped = false
                    current_binary = +""
                  end
                end
              else
                strings[count] << c
              end
            end
          end
          pos += 1
        }
        finish_strings.call
      end

      def TXT.display(str, do_escapes = true)
        output = "".b
        str.each_byte do |octet|
          if octet == 34 || octet == 92
            output << 92 if do_escapes
            output << octet
          elsif octet < 32 && !ESCAPE_CODES.key?(octet)
            output << format('\\%03d', octet)
          else
            output << octet
          end
        end
        output
      end

      def rdata_to_string
        if (defined?@strings)
          temp = []
          @strings.each {|str|
            output = TXT.display(str)
            temp.push("\"#{output}\"")
          }
          return temp.join(' ')
        end
        return ''
      end

      def encode_rdata(msg, canonical=false) #:nodoc: all
        msg.put_string_list(@strings)
      end

      def self.decode_rdata(msg) #:nodoc: all
        strings = msg.get_string_list
        return self.new(strings)
      end
    end
  end
end
