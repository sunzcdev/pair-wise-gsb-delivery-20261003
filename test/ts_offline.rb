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

require_relative 'spec_helper'

Dnsruby.log.level = Logger::FATAL

# We'll prepend 'tc_' and append '.rb' to these:
TESTS = %w(
    dnskey
    cache_keys
    attempt_configuration
    caa
    review_record_compatibility
    review_validation_priming
    review_cache
    escapedchars
    edns_retry_regressions
    gpos
    generic_cache
    hash
    header
    ipseckey
    ipv6
    malformed_tcp
    pipeline_send
    pipeline_retirement
    pipeline_retry
    resolver_concurrency
    resolver_validation
    release_transport
    release_transport_fixes
    v2_release_transport
    v2_zone_transfer_native
    release_negative_cache
    release_recursor_cache
    release_zone_ttls
    release_zone_defaults
    release_configuration_fixes
    release_auxiliary_fixes
    recursor_address_fallback
    recursor_limits
    resolver_configuration_contracts
    release_review_transport
    release_review_transport_native
    review_transport_fixes
    review_control_deadlines
    review_followup_transport
    review_transport_native
    review_packet_contracts
    final_review_fixes
    final_transport_contracts
    protocol_review_fixes
    rrset_review_fixes
    dnssec_recursor_review_fixes
    cancelled_send_capacity
    review_code_mapper
    review_presentation
    record_regressions
    record_codec_contracts
    selector_wakeup
    message
    misc
    name
    nameserver_bootstrap
    naptr
    nsec
    nsec3
    nsec3param
    nxt
    tlsa
    packet
    packet_unique_push
    ptrin
    question
    query_control
    res_config
    res_file
    res_opt
    scoped_ipv6
    scoped_ipv6_select
    scoped_ipv6_transport
    rr
    rr-txt
    rr-unknown
    rrset
    rrset_grouping
    rrsig
    svcb
    svcb_transport
    socket_helpers
    tcp_pipelining
    transport_fork
    tkey
    tsig_api
    tsig_rfc8945
    update
    zone_reader
)

# Omitted:
#
# tc_res_env


TESTS.each { |test| require_relative "tc_#{test}.rb" }


def have_open_ssl?
  have_open_ssl = true
  begin
    require "openssl"
    OpenSSL::Digest::SHA256.digest("data")
  rescue LoadError, StandardError
    have_open_ssl = false
  end
  have_open_ssl
end

if have_open_ssl?
  require_relative 'tc_ds.rb'
  require_relative 'tc_svcb_signed_target.rb'
  require_relative 'tc_dnssec_trust_anchors.rb'
  require_relative 'tc_release_dnssec_fixes.rb'
  require_relative 'tc_release_dnssec_completion.rb'
  require_relative 'tc_v2_release_dnssec.rb'
  require_relative 'tc_dnssec_trust_races.rb'
  require_relative 'tc_key_cache_identity.rb'
  require_relative 'tc_dnssec_budget.rb'
  require_relative 'tc_dnssec_recursion_limits.rb'
  require_relative 'tc_supporting_lookup_limits.rb'
  require_relative 'tc_dnssec_cache_lifetime.rb'
  require_relative 'tc_dnssec_cache_bounds.rb'
  require_relative 'tc_dnssec_wildcard_proofs.rb'
  require_relative 'tc_dnssec_alias_validation.rb'
  require_relative 'tc_dnssec_denial_proofs.rb'
else
  puts "-----------------------------------------------------------------------"
  puts "OpenSSL SHA256 unavailable - skipping DS and DNSSEC trust-anchor tests"
  puts "-----------------------------------------------------------------------"
end
