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
# See the License f181or the specific language governing permissions and
# limitations under the License.
# ++
require 'digest/sha2'
require 'dnsruby/key_cache'
require 'dnsruby/validation_budget'
require 'dnsruby/single_verifier'
module Dnsruby

  #  RFC4033, section 7
  #    "There is one more step that a security-aware stub resolver can take
  #    if, for whatever reason, it is not able to establish a useful trust
  #    relationship with the recursive name servers that it uses: it can
  #    perform its own signature validation by setting the Checking Disabled
  #    (CD) bit in its query messages.  A validating stub resolver is thus
  #    able to treat the DNSSEC signatures as trust relationships between
  #    the zone administrators and the stub resolver itself. "
  # 
  #  Resolver queries do not validate responses by default; set
  #  Resolver#do_validation to true to enable validation. Root DS trust anchors
  #  are configured below. Applications may also use verify() to verify RRSets
  #  or Messages with given keys, or add their own trust anchors to this class.
  # 
  #  Messages are tagged with the current security_level (Message::SecurityLevel).
  #  UNCHECKED means Dnsruby has not attempted to validate the response.
  #  BOGUS means the response has been checked, and is bogus.
  #  INSECURE means the response has been validated to be insecure (e.g. in an unsigned zone)
  #  SECURE means that the response has been verfied to be correct.
  # 
  #  Several validators are provided, with each maintaining its own cache of trusted keys.
  #  If validators are added or removed, the caches of the other validators are not affected.
  class Dnssec
    # Limits apply to one complete validation, including trust-chain discovery.
    # A limit is a VerifyError; it never downgrades a signed answer to insecure.
    @validation_limits = ValidationBudget::DEFAULT_LIMITS
    def self.validation_limits
      @validation_limits
    end

    def self.validation_limits=(limits)
      unknown = limits.keys - ValidationBudget::DEFAULT_LIMITS.keys
      raise ArgumentError, "Unknown DNSSEC validation limits: #{unknown.join(', ')}" unless unknown.empty?
      merged = ValidationBudget::DEFAULT_LIMITS.merge(limits)
      merged.each do |key, value|
        valid = key == :timeout ? value.is_a?(Numeric) && value.finite? && value > 0 : value.is_a?(Integer) && value > 0
        raise ArgumentError, "Invalid DNSSEC validation limit #{key}" unless valid
      end
      @validation_limits = merged.freeze
    end
    #  A class to cache trusted keys


    class ValidationPolicy
      #  @TODO@ Could do this by getting client to add verifiers in the order they
      #  want them to be used. Could then dispense with all this logic
      # Configured DLV registries are a final fallback for the two ordered
      # policies, after root/local anchors, when those paths are not bogus.
      # The two ONLY policies never consult another source of trust.

      # * Always use the root and ignore local trust anchors.
      ALWAYS_ROOT_ONLY = 1
      # * Use the root if successful, otherwise try local anchors.
      ROOT_THEN_LOCAL_ANCHORS = 2
      # * Use local trust anchors if available, otherwise use root.
      LOCAL_ANCHORS_THEN_ROOT = 3
      # * Always use local trust anchors and ignore the root.
      ALWAYS_LOCAL_ANCHORS_ONLY = 4
    end
    @@validation_policy = ValidationPolicy::LOCAL_ANCHORS_THEN_ROOT

    def Dnssec.validation_policy=(p)
      if ((p >= ValidationPolicy::ALWAYS_ROOT_ONLY) && (p <= ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY))
        @@validation_policy = p
        #  @TODO@ Should we be clearing the trusted keys now?
      end
    end
    def Dnssec.validation_policy
      @@validation_policy
    end

    #  #NOTE# You may wish to import these via a secure channel yourself, if
    #  using Dnsruby for validation.
    #  KSK-2017 and KSK-2024, published at https://data.iana.org/root-anchors/root-anchors.xml
    ROOT_TRUST_ANCHORS = [
      ". IN DS 20326 8 2 E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D".freeze,
      ". IN DS 38696 8 2 683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16".freeze
    ].freeze

    @@root_verifier = SingleVerifier.new(SingleVerifier::VerifierType::ROOT)
    ROOT_TRUST_ANCHORS.each {|ds| @@root_verifier.add_root_ds(RR.create(ds))}

    @@dlv_verifier = SingleVerifier.new(SingleVerifier::VerifierType::DLV)

    #  @TODO@ Could add a new one of these for each anchor.
    @@anchor_verifier = SingleVerifier.new(SingleVerifier::VerifierType::ANCHOR)


    #  Add a trusted Key Signing Key for the ISC DLV registry.
    def Dnssec.add_dlv_key(dlv_key)
      @@dlv_verifier.add_dlv_key(dlv_key)
    end
    #  Add a new trust anchor
    def Dnssec.add_trust_anchor(t)
      #  @TODO@ Create a new verifier?
      @@anchor_verifier.add_trust_anchor(t)
    end
    #  Add the trusted key with the given expiration time
    def self.add_trust_anchor_with_expiration(k, expiration)
      #  Create a new verifier?
      @@anchor_verifier.add_trust_anchor_with_expiration(k, expiration)
    end
    #  Remove a configured trust anchor and clear keys learned from local anchors.
    def Dnssec.remove_trust_anchor(t)
      @@anchor_verifier.remove_trust_anchor(t)
    end
    #  Remove all local trust anchors and keys learned from them.
    #  Built-in root anchors are unaffected.
    def self.clear_trust_anchors
      @@anchor_verifier.clear_trust_anchors
    end

    def self.trust_anchors
      return @@anchor_verifier.trust_anchors
    end

    #  Clear learned keys and all DS/DLV anchors, including the built-in root DS
    #  anchors. Configured DNSKEY anchors are retained.
    def self.clear_trusted_keys
      [@@anchor_verifier, @@root_verifier, @@dlv_verifier].each {|v|
        v.clear_trusted_keys
      }
    end

    def self.reset
      SingleVerifier.set_recursor_hints(nil)
      @@validation_policy = ValidationPolicy::LOCAL_ANCHORS_THEN_ROOT
      @@root_verifier = SingleVerifier.new(SingleVerifier::VerifierType::ROOT)
      ROOT_TRUST_ANCHORS.each {|ds| @@root_verifier.add_root_ds(RR.create(ds))}

      @@dlv_verifier = SingleVerifier.new(SingleVerifier::VerifierType::DLV)

      #  @TODO@ Could add a new one of these for each anchor.
      @@anchor_verifier = SingleVerifier.new(SingleVerifier::VerifierType::ANCHOR)
      @@do_validation_with_recursor = true # Many nameservers don't handle DNSSEC correctly yet
      @@default_resolver = Resolver.new
    end

    def self.set_hints(hints)
      SingleVerifier.set_recursor_hints(hints)
    end

    def self.no_keys?
      no_keys = true
      [@@anchor_verifier, @@root_verifier, @@dlv_verifier].each {|v|
        if (v.trusted_keys.length() > 0 ||
              v.trust_anchors.length() > 0)
          no_keys = false
        end
      }
      return no_keys
    end

    @@do_validation_with_recursor = true # Many nameservers don't handle DNSSEC correctly yet
    @@default_resolver = Resolver.new
    #  This method defines the choice of Resolver or Recursor, when the validator
    #  is checking responses.
    #  If set to true, then a Recursor will be used to query for the DNSSEC records.
    #  Otherwise, the default system resolver will be used.
    def self.do_validation_with_recursor(on)
      @@do_validation_with_recursor = on
    end
    def self.do_validation_with_recursor?
      return @@do_validation_with_recursor
    end
    #  This method overrides the system default resolver configuration for validation
    #  If default_resolver is set, then it will be used to follow the chain of trust.
    #  If it is not, then the default system resolver will be used (unless do_validation_with_recursor
    #  is set.
    def self.default_resolver=(res)
      @@default_resolver = res
    end
    def self.default_resolver
      return @@default_resolver
    end

    #  Returns true for secure/insecure, false otherwise
    #  This method will set the security_level on msg to the appropriate value.
    #  Could be : secure, insecure, bogus or indeterminate
    #  If an error is encountered during verification, then the thrown exception
    #  will define the error.
    def self.validate(msg)
      query = Message.new()
      query.header.cd=true
      validate_with_query(query, msg)
    end

    def self.validate_with_query(query, msg)
      ValidationBudget.with { validate_with_query_internal(query, msg) }
    end

    private_class_method def self.validate_with_query_internal(query, msg)
      if (!msg)
        return false
      end
      #  First, just check there is something to validate!
      found_sigs = false
      msg.each_resource {|rr|
        if (rr.type == Types::RRSIG)
          found_sigs = true
        end
      }
      if (found_sigs)
        begin
          result = verify_result(msg, nil, operational: true)
          if result == :secure
            msg.security_level = Message::SecurityLevel.SECURE
            msg.security_error = ''
            return true
          end
        rescue ValidationLimitError
          raise
        rescue VerifyError
          # Missing cached keys and a failed trusted path both arrive here.
          # Full validation discovers the applicable anchors and distinguishes
          # those cases. A cached INSECURE result must go through that policy
          # too: another applicable anchor may establish SECURE or BOGUS.
        end
      end

      #  SHOULD ALWAYS VERIFY DNSSEC-SIGNED RESPONSES?
      #  Yes - if a trust anchor is configured. Otherwise, act on CD bit (in query)
      TheLog.debug("Checking whether to validate, query.cd = #{query.header.cd}")
      if (((@@validation_policy > ValidationPolicy::ALWAYS_ROOT_ONLY) && (self.trust_anchors().length > 0)) ||
            #  Check query here, and validate if CD is true
          ((query.header.cd == true))) # && (query.do_validation)))
        TheLog.debug("Starting validation")

        #  Validate!
        #  Need to think about trapping/storing exceptions and security_levels here
        last_error = ""
        last_level = Message::SecurityLevel.BOGUS
        last_error_level = Message::SecurityLevel.BOGUS
        if (@@validation_policy == ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY)
          last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
            Proc.new{|m, q| validate_with_anchors(m, q)}, msg, query)
        elsif (@@validation_policy == ValidationPolicy::ALWAYS_ROOT_ONLY)
          last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
            Proc.new{|m, q| validate_with_root(m, q)}, msg, query)
        elsif (@@validation_policy == ValidationPolicy::LOCAL_ANCHORS_THEN_ROOT)
          last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
            Proc.new{|m, q| validate_with_anchors(m, q)}, msg, query)
          if (last_level != Message::SecurityLevel.SECURE)
            last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
              Proc.new{|m, q| validate_with_root(m, q)}, msg, query)
          end
        elsif (@@validation_policy == ValidationPolicy::ROOT_THEN_LOCAL_ANCHORS)
          last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
            Proc.new{|m, q| validate_with_root(m, q)}, msg, query)
          if (last_level != Message::SecurityLevel.SECURE)
            last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
              Proc.new{|m, q| validate_with_anchors(m, q)}, msg, query)
          end
        end
        if (last_level != Message::SecurityLevel.SECURE && last_level != Message::SecurityLevel.BOGUS) &&
            [ValidationPolicy::LOCAL_ANCHORS_THEN_ROOT, ValidationPolicy::ROOT_THEN_LOCAL_ANCHORS].include?(@@validation_policy)
          last_level, last_error, last_error_level = try_validation(last_level, last_error, last_error_level,
            Proc.new{|m, q| validate_with_dlv(m, q)}, msg, query)
        end
        #  Set the message security level!
        if last_level == Message::SecurityLevel.SECURE
          last_error = ''
        elsif last_error && !last_error.empty?
          last_level = Message::SecurityLevel.BOGUS
        end
        msg.security_level = last_level
        msg.security_error = last_error
        msg.header.ad = false if msg.security_level == Message::SecurityLevel.INSECURE
        raise VerifyError.new(last_error) if (msg.security_level < 0)
        return (msg.security_level.code > Message::SecurityLevel::UNCHECKED)
      end
      msg.security_level = Message::SecurityLevel.UNCHECKED
      return true
    end

    def self.try_validation(last_level, last_error, last_error_level, proc, msg, query)   # :nodoc:
      ValidationBudget.with do
        try_validation_internal(last_level, last_error, last_error_level, proc, msg, query)
      end
    end

    private_class_method def self.try_validation_internal(last_level, last_error, last_error_level, proc, msg, query)
      # Keep the lifetime with the successful result. A later unanchored
      # verifier can return INSECURE without establishing a cacheable proof.
      previous_expiration = if last_level == Message::SecurityLevel.INSECURE
        msg.instance_variable_get(:@dnsruby_authenticated_expiration)
      end
      begin
        result = proc.call(msg, query)
        if !result || msg.security_level == Message::SecurityLevel.BOGUS
          detail = msg.security_error.to_s
          raise VerifyError, detail.empty? ? 'DNSSEC validation failed' : detail
        end
        if previous_expiration && msg.security_level <= Message::SecurityLevel.INSECURE
          current_expiration = msg.instance_variable_get(:@dnsruby_authenticated_expiration)
          msg.instance_variable_set(:@dnsruby_authenticated_expiration,
            [previous_expiration, current_expiration].compact.min)
        end
        last_level = Message::SecurityLevel.new([msg.security_level.code, last_level.code].max)
      rescue VerifyError => e
        # A failed trusted path cannot be erased by a later unanchored or
        # insecure result. A separately authenticated SECURE path may win.
        if last_error.to_s.empty? || last_error_level < last_level
          last_error = e.to_s
          last_error_level = last_level
        end
      end
      return last_level, last_error, last_error_level
    end

    def self.validate_with_anchors(msg, query)
      @@anchor_verifier.validate(msg, query)
    end

    def self.validate_with_root(msg, query)
      @@root_verifier.validate(msg, query)
    end

    def self.validate_with_dlv(msg, query)
      @@dlv_verifier.validate(msg, query)
    end

    def self.verify(msg, keys=nil)
      verify_result(msg, keys) == :secure
    end

    # Keep the result separate from mutable Message state: a previous validation
    # must not turn a newly verified Opt-Out response into a secure response.
    private_class_method def self.verify_result(msg, keys=nil, operational: false)
      ValidationBudget.with { verify_internal(msg, keys, operational: operational) }
    end

    private_class_method def self.verify_internal(msg, keys, operational: false)
      insecure = false
      insecure_expiration = nil
      last_error = nil
      verifiers = if operational
        case @@validation_policy
        when ValidationPolicy::ALWAYS_ROOT_ONLY then [@@root_verifier]
        when ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY then [@@anchor_verifier]
        when ValidationPolicy::ROOT_THEN_LOCAL_ANCHORS then [@@root_verifier, @@anchor_verifier]
        else [@@anchor_verifier, @@root_verifier]
        end
      else
        [@@anchor_verifier, @@root_verifier, @@dlv_verifier]
      end
      verifiers.each do |verifier|
        begin
          result = operational ? verifier.verify_for_validation(msg) : verifier.verify_result(msg, keys)
          return :secure if result == :secure
          if result == :insecure
            insecure = true
            expiration = msg.instance_variable_get(:@dnsruby_authenticated_expiration)
            insecure_expiration = [insecure_expiration, expiration].compact.min
          end
        rescue ValidationLimitError
          raise
        rescue VerifyError => error
          last_error = error
        end
      end
      if insecure
        msg.instance_variable_set(:@dnsruby_authenticated_expiration, insecure_expiration) if insecure_expiration
        return :insecure
      end
      raise last_error if last_error
      false
    end

    def self.anchor_verifier
      return @@anchor_verifier
    end
    def self.dlv_verifier
      return @@dlv_verifier
    end
    def self.root_verifier
      return @@root_verifier
    end




    def self.verify_rrset(rrset, keys = nil)
      ValidationBudget.with do
        last_error = nil
        verified = [@@anchor_verifier, @@root_verifier, @@dlv_verifier].any? do |verifier|
          ValidationBudget.current.check!
          begin
            verifier.verify_rrset(rrset, keys)
          rescue ValidationLimitError
            raise
          rescue VerifyError => error
            last_error = error
            false
          end
        end
        raise last_error if !verified && last_error
        verified
      end
    end
  end
end
