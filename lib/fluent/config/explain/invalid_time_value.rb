#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

module Fluent
  module Config
    module Explain
      # A time value that could not be parsed. It carries the same source
      # information as a TimeExplanation so the offending directive can be
      # located, together with the underlying error message.
      class InvalidTimeValue
        attr_reader :raw_value, :name, :source, :message

        def initialize(raw_value, name, source, message)
          @raw_value = raw_value
          @name = name
          @source = source
          @message = message
        end

        def valid?
          false
        end

        def to_h
          {
            'raw_value' => @raw_value,
            'name' => @name,
            'source' => @source.to_h,
            'error' => @message,
          }
        end
      end
    end
  end
end
