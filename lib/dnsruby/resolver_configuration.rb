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
  class Resolver
    # One immutable, published Resolver configuration.
    #
    # A query captures the published ResolverConfiguration when it is admitted
    # and keeps it for its whole lifetime: nameservers, retry/timeout policy and
    # validation settings cannot change underneath an in-flight request. Later
    # queries observe the next published configuration.
    #
    # The snapshot stores only plain values: strings, numbers, booleans, arrays
    # and hashes. It never holds a PacketSender, an open socket, a mutex or an
    # SSL/TSIG object, so publishing and inspecting it cannot share mutable I/O
    # state and the value part remains serializable. Sender objects needed to
    # dispatch the captured nameservers are associated separately at admission.
    class ResolverConfiguration
      # Plain-value description of one configured nameserver.
      Server = Struct.new(:server, :port, keyword_init: true) do
        def initialize(*)
          super
          self.server = server.to_s.dup.freeze
          self.port = Integer(port)
          freeze
        end
      end

      # Plain-value description of TSIG policy, or nil when signing is off.
      Tsig = Struct.new(:name, :key, :algorithm, keyword_init: true) do
        def initialize(*)
          super
          self.name = name.to_s.dup.freeze
          self.key = key.nil? ? nil : key.dup.freeze
          self.algorithm = algorithm.to_s.dup.freeze
          freeze
        end
      end

      ATTRIBUTES = %i[
        port use_tcp no_tcp tcp_pipelining tcp_pipelining_max_queries tcp_idle_timeout
        ignore_truncation src_address src_address6 src_port recurse udp_size dnssec
        do_validation do_caching packet_timeout query_timeout retry_times retry_delay
      ].freeze

      attr_reader :servers, :tsig, :generation, :publisher, *ATTRIBUTES

      def initialize(attributes, servers:, tsig:, generation:, publisher: nil)
        ATTRIBUTES.each do |attribute|
          value = attributes.fetch(attribute)
          value = value.dup.freeze if value.is_a?(String) || value.is_a?(Array) || value.is_a?(Hash)
          instance_variable_set("@#{attribute}", value)
        end
        @servers = servers.map { |server| server.is_a?(Server) ? server : Server.new(server) }.freeze
        @tsig = tsig.is_a?(Tsig) ? tsig : (tsig && Tsig.new(tsig))
        @generation = Integer(generation)
        # The live Resolver is identity only, not serialized snapshot data.
        @publisher = publisher
        freeze
      end

      def nameservers
        servers.map(&:server)
      end

      # Plain-value representation suitable for inspection and serialization.
      # The live publisher Resolver is deliberately excluded.
      def to_h
        ATTRIBUTES.each_with_object(generation: generation, servers: servers.map(&:to_h),
          tsig: tsig && tsig.to_h) do |attribute, result|
          result[attribute] = public_send(attribute)
        end
      end
    end
  end
end
