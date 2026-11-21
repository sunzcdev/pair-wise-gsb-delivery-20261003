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

require 'fluent/config/types'
require 'fluent/config/error'

module Fluent
  module Config
    module Explain
      # Break-down of a time parameter value, computed with the exact same
      # rules as Fluent::Config.time_value so that an explanation never drifts
      # from what Fluentd actually applies.
      #
      # A TimeExplanation reports:
      #   * raw_value: the value found in the configuration (after ERB /
      #     embedded Ruby substitution), before any numeric conversion
      #   * unit:      the unit that was actually applied
      #   * seconds:   the converted numeric value, identical to
      #                Fluent::Config.time_value(raw_value)
      #
      # Historical bare-number spellings (e.g. "60", "0.5") carry no unit and
      # are interpreted as seconds; this is reported explicitly instead of
      # silently looking identical to an explicit "60s".
      class TimeExplanation
        # Multipliers mirror the case-sensitive /.../ branches in
        # Fluent::Config.time_value. Units are lowercase only: a capital
        # suffix ("10S") does not match there and falls through to the
        # bare-number path below.
        UNIT_SECONDS = {
          's' => 1,
          'm' => 60,
          'h' => 60 * 60,
          'd' => 24 * 60 * 60,
        }.freeze

        BARE_NUMBER_UNIT = nil # historical spelling: number without a unit, read as seconds

        attr_reader :raw_value, :unit, :seconds, :source

        def initialize(raw_value, unit, seconds, source)
          @raw_value = raw_value
          @unit = unit
          @seconds = seconds
          @source = source
        end

        def explicit_unit?
          !@unit.nil?
        end

        # "60" (no unit) is the v0.12-era spelling that time_value reads as a
        # bare Float in seconds.
        def bare_number?
          @unit.nil?
        end

        def to_h
          {
            'raw_value' => @raw_value,
            'unit' => @unit,
            'seconds' => @seconds,
            'source' => @source.to_h,
          }
        end

        def inspect
          "#<#{self.class.name} raw_value=#{@raw_value.inspect} unit=#{@unit.inspect} seconds=#{@seconds.inspect} source=#{@source.inspect}>"
        end

        # Explains a single already-substituted string value.
        #
        # @param value [String, Numeric] value as stored in the config element
        # @param name  [String, nil] parameter name, used in error messages
        # @param source [Fluent::Config::Explain::ParamSource] where the value came from
        # @raise [Fluent::ConfigError] when the value looks like a suffixed
        #   duration but uses an unknown unit, e.g. "10x" or "10S"
        def self.explain(value, name: nil, source: nil)
          str = value.to_s

          if (match = str.match(/\A([0-9]+)([smhd])\z/))
            number = match[1].to_i
            unit = match[2]
            seconds = number * UNIT_SECONDS[unit]
            new(str, unit, seconds, source)
          elsif str.match?(/\A-?[0-9]+(\.[0-9]+)?\z/)
            # Historical no-unit spelling. Keep Float semantics identical to
            # the FLOAT_TYPE fall-through in Fluent::Config.time_value.
            seconds = str.to_f
            new(str, BARE_NUMBER_UNIT, seconds, source)
          else
            # Anything else is either an unknown suffix ("10x", "10sec",
            # "10S") or non-numeric text. time_value would currently swallow
            # these via String#to_f; surface them instead so the mistake can be
            # located and fixed.
            label = name ? "'#{name}'" : 'time parameter'
            raise Fluent::ConfigError,
                  "#{label}: invalid time value #{str.inspect}: expected a number with one of s/m/h/d unit (e.g. \"10s\") or a bare number in seconds"
          end
        end
      end
    end
  end
end
