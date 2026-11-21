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
      # Where a parameter value came from:
      #   * kind:    :explicit (written in the config) or :default (plugin default)
      #   * section: human-readable path of directives, e.g.
      #              ["match test.**", "buffer"]
      #   * plugin:  plugin class name owning the parameter, e.g.
      #              "Fluent::Plugin::StdoutOutput"
      #   * name:    parameter name as written in the config
      ParamSource = Struct.new(:kind, :section, :plugin, :name) do
        def explicit?
          kind == :explicit
        end

        def default?
          kind == :default
        end

        def section_path
          section.join(' > ')
        end

        def to_h
          {
            'kind' => kind.to_s,
            'section' => section,
            'section_path' => section_path,
            'plugin' => plugin,
            'name' => name,
          }
        end
      end
    end
  end
end
