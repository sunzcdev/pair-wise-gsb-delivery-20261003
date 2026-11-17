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
module Dnsruby
  class RR
    # RFC4034, section 2
    # DNSSEC uses public key cryptography to sign and authenticate DNS
    # resource record sets (RRsets).  The public keys are stored in DNSKEY
    # resource records and are used in the DNSSEC authentication process
    # described in [RFC4035]: A zone signs its authoritative RRsets by
    # using a private key and stores the corresponding public key in a
    # DNSKEY RR.  A resolver can then use the public key to validate
    # signatures covering the RRsets in the zone, and thus to authenticate
    # them.
    class DNSKEY < RR
      ClassValue = nil #:nodoc: all
      TypeValue = Types::DNSKEY #:nodoc: all

      # Key is revoked
      REVOKED_KEY = 0x80

      # Key is a zone key
      ZONE_KEY = 0x100

      # Key is a secure entry point key
      SEP_KEY = 0x1

      # The flags for the DNSKEY RR
      attr_reader :flags
      # The protocol for this DNSKEY RR.
      # MUST be 3.
      attr_reader :protocol
      # The algorithm used for this key
      # See Dnsruby::Algorithms for permitted values
      attr_reader :algorithm
      # The public key
      attr_reader :key
      # The length (in bits) of the key - NOT key.length
      attr_reader :key_length

      def init_defaults
        @make_new_key_tag = false
        self.protocol=3
        self.flags=ZONE_KEY
        @algorithm=Algorithms.RSASHA1
        @public_key = nil
        @key_tag = nil
        @make_new_key_tag = true
      end

      def protocol=(p)
        if (p!=3)
          raise DecodeError.new("DNSKEY protocol field set to #{p}, contrary to RFC4034 section 2.1.2")
        else @protocol = p
        end
        get_new_key_tag
      end

      def algorithm=(a)
        if (a.instance_of?String)
          if (a.to_i > 0)
            a = a.to_i
          end
        end
        begin
          alg = Algorithms.new(a)
          @algorithm = alg
          @public_key = @key_length = nil
        rescue ArgumentError => e
          raise DecodeError.new(e)
        end
        get_new_key_tag
      end

      def revoked=(on)
        if (on)
          @flags |= REVOKED_KEY
        else
          @flags &= (~REVOKED_KEY)
        end
        get_new_key_tag
      end

      def revoked?
        return ((@flags & REVOKED_KEY) > 0)
      end

      def zone_key=(on)
        if (on)
          @flags |= ZONE_KEY
        else
          @flags &= (~ZONE_KEY)
        end
        get_new_key_tag
      end

      def zone_key?
        return ((@flags & ZONE_KEY) > 0)
      end

      def sep_key=(on)
        if (on)
          @flags |= SEP_KEY
        else
          @flags &= (~SEP_KEY)
        end
        get_new_key_tag
      end

      def sep_key?
        return ((@flags & SEP_KEY) > 0)
      end

      def flags=(f)
        #  Only three values allowed -
        #  Zone Key flag (bit 7)
        #  Secure Entry Point flag (bit 15)
        #  Revoked bit (bit 8) - RFC 5011
        if ((f & ~ZONE_KEY & ~SEP_KEY & ~REVOKED_KEY) > 0)
          TheLog.info("DNSKEY: Only zone key, secure entry point and revoked flags allowed for DNSKEY" +
              " (RFC4034 section 2.1.1) : #{f} entered as input")
        end

        @flags = f
        get_new_key_tag
      end

      #       def bad_flags?
      #         if ((@flags & ~ZONE_KEY & ~SEP_KEY) > 0)
      #           return true
      #         end
      #         return false
      #       end
      # 
      def from_data(data) #:nodoc: all
        flags, protocol, algorithm, @key = data
        @make_new_key_tag = false
        self.flags=(flags)
        self.protocol=(protocol)
        self.algorithm=(algorithm)
        @make_new_key_tag = true
        get_new_key_tag
      end

      def from_hash(hash) #:nodoc: all
        @make_new_key_tag = false
        hash.keys.each do |param|
          send(param.to_s+"=", hash[param])
        end
        @make_new_key_tag = true
        get_new_key_tag
      end

      def from_string(input)
        if (input.length > 0)
          @make_new_key_tag = false
          data = input.split(" ")
          self.flags=(data[0].to_i)
          self.protocol=(data[1].to_i)
          self.algorithm=(data[2])
          #  key can include whitespace - include all text
          #  until we come to " )" at the end, and then gsub
          #  the white space out
          #  Also, brackets may or may not be present
          #  Not to mention comments! ";"
          buf = ""
          index = 3
          end_index = data.length - 1
          if (data[index]=="(")
            end_index = data.length - 2
            index = 4
          end
          (index..end_index).each {|i|
            if (comment_index = data[i].index(";"))
              buf += data[i].slice(0, comment_index)
              #  @TODO@ We lose the comments here - we should really keep them for when we write back to string format?
              break
            else
              buf += data[i]
            end
          }
          self.key=(buf)
          @make_new_key_tag = true
          get_new_key_tag
        end
      end

      def rdata_to_string #:nodoc: all
        if (@flags!=nil)
          #           return "#{@flags} #{@protocol} #{@algorithm.string} ( #{Base64.encode64(@key.to_s)} )"
          return "#{@flags} #{@protocol} #{@algorithm.string} ( #{[@key.to_s].pack("m*").gsub("\n", "")} ) ; key_tag=#{key_tag}"
        else
          return ""
        end
      end

      def encode_rdata(msg, canonical=false) #:nodoc: all
        #  2 octets, then 2 sets of 1 octet
        msg.put_pack('nCC', @flags, @protocol, @algorithm.code)
        msg.put_bytes(@key)
      end

      def self.decode_rdata(msg) #:nodoc: all
        #  2 octets, then 2 sets of 1 octet
        flags, protocol, algorithm = msg.get_unpack('nCC')
        key = msg.get_bytes
        return self.new(
          [flags, protocol, algorithm, key])
      end

      #  Return the the key tag this key would have had before it was revoked
      #  If the key is not revoked, then the current key_tag will be returned
      def key_tag_pre_revoked
        if (!revoked?)
          return key_tag
        end
        new_key = clone
        new_key.revoked = false
        return new_key.key_tag
      end

      def get_new_key_tag
        if (@make_new_key_tag)
          rdata = MessageEncoder.new {|msg|
            encode_rdata(msg)
          }.to_s
          tag = generate_key_tag(rdata, @algorithm)
          @key_tag = tag
        end
      end

      #  Return the tag for this key
      def key_tag
        if (!@key_tag)
          @make_new_key_tag = true
          get_new_key_tag
        end
        return @key_tag
      end

      def generate_key_tag(rdata, algorithm)
        tag=0
        if (algorithm == Algorithms.RSAMD5)
          # The key tag for algorithm 1 (RSA/MD5) is defined differently from the
          # key tag for all other algorithms, for historical reasons.
          d1 = rdata[rdata.length - 3] & 0xFF
          d2 = rdata[rdata.length - 2] & 0xFF
          tag = (d1 << 8) + d2
        else
          tag = 0
          last = 0
          0.step(rdata.length - 1, 2) {|i|
            last = i
            d1 = rdata[i]
            d2 = rdata[i + 1] || 0 # odd number of bytes possible

            d1 = d1.getbyte(0) if d1.class == String # Ruby 1.9
            d2 = d2.getbyte(0) if d2.class == String # Ruby 1.9

            d1 = d1  & 0xFF
            d2 = d2  & 0xFF

            tag += ((d1 << 8) + d2)
          }
          last+=2
          if (last < rdata.length)
            d1 = rdata[last]

            if (d1.class == String) # Ruby 1.9
              d1 = d1.getbyte(0)
            end

            d1 = d1 & 0xFF
            tag += (d1 << 8)
          end
          tag += ((tag >> 16) & 0xFFFF)
        end
        tag=tag&0xFFFF
        return tag
      end

      def key=(key_text)
        begin
          @key=Base64.decode64(key_text.delete("\n "))
          @public_key = @key_length = nil
          public_key
          get_new_key_tag
        rescue Exception => e
          Dnsruby.log.error(e)
          raise ArgumentError.new("Key #{key_text} invalid")
        end
      end

      def public_key
        if (!@public_key)
          if [Algorithms.RSASHA1,
              Algorithms.RSASHA256,
              Algorithms.RSASHA512,
              Algorithms.RSASHA1_NSEC3_SHA1].include?(@algorithm)
            @public_key = rsa_key
          elsif [Algorithms.DSA,
              Algorithms.DSA_NSEC3_SHA1].include?(@algorithm)
            @public_key = dsa_key
          elsif [Algorithms.ECDSAP256SHA256, Algorithms.ECDSAP384SHA384].include?(@algorithm)
            @public_key = ec_key(Algorithms.ECDSAP256SHA256 == @algorithm ? 'prime256v1' : 'secp384r1')
          elsif @algorithm == Algorithms.ED25519 && DNSKEY.ed25519_supported?
            @public_key = ed25519_key
          elsif @algorithm == Algorithms.ED448 && DNSKEY.ed448_supported?
            @public_key = ed448_key
          end
        end
        #  @TODO@ Support other key encodings!
        return @public_key
      end

      # Check the actual provider as well as Ruby's OpenSSL API. Parsing a
      # public key alone does not guarantee that signature verification works.
      def self.ed25519_supported? #:nodoc:
        return @ed25519_supported if defined?(@ed25519_supported)
        return false unless defined?(OpenSSL::PKey)
        public_bytes = ['d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'].pack('H*')
        signature = ['e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155' \
          '5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b'].pack('H*')
        # RFC 8032 section 7.1, test 1 (the empty message).
        key = OpenSSL::PKey.read(ed25519_subject_public_key(public_bytes))
        @ed25519_supported = key.verify(nil, signature, ''.b)
      rescue OpenSSL::OpenSSLError, NotImplementedError, TypeError, ArgumentError
        @ed25519_supported = false
      end

      def self.ed25519_subject_public_key(bytes) #:nodoc:
        # RFC 8410 SubjectPublicKeyInfo: Ed25519's algorithm has no parameters.
        OpenSSL::ASN1::Sequence.new([
          OpenSSL::ASN1::Sequence.new([OpenSSL::ASN1::ObjectId.new('1.3.101.112')]),
          OpenSSL::ASN1::BitString.new(bytes)
        ]).to_der
      end
      private_class_method :ed25519_subject_public_key

      def ed25519_key
        unless @key.bytesize == 32
          raise OpenSSL::PKey::PKeyError, 'Ed25519 DNSKEY must contain 32 octets'
        end
        @key_length = 256
        OpenSSL::PKey.read(DNSKEY.send(:ed25519_subject_public_key, @key))
      end

      def self.ed448_supported? #:nodoc:
        return @ed448_supported if defined?(@ed448_supported)
        return false unless defined?(OpenSSL::PKey)
        # RFC 8032 section 7.4, the empty-message test vector.
        public_bytes = ['5fd7449b59b461fd2ce787ec616ad46a' \
          '1da1342485a70e1f8a0ea75d80e96778' \
          'edf124769b46c7061bd6783df1e50f6c' \
          'd1fa1abeafe8256180'].pack('H*')
        signature = ['533a37f6bbe457251f023c0d88f976ae' \
          '2dfb504a843e34d2074fd823d41a591f' \
          '2b233f034f628281f2fd7a22ddd47d78' \
          '28c59bd0a21bfd3980ff0d2028d4b18a' \
          '9df63e006c5d1c2d345b925d8dc00b41' \
          '04852db99ac5c7cdda8530a113a0f4db' \
          'b61149f05a7363268c71d95808ff2e65' \
          '2600'].pack('H*')
        key = OpenSSL::PKey.read(ed448_subject_public_key(public_bytes))
        @ed448_supported = key.verify(nil, signature, ''.b)
      rescue OpenSSL::OpenSSLError, NotImplementedError, TypeError, ArgumentError
        @ed448_supported = false
      end

      def self.ed448_subject_public_key(bytes) #:nodoc:
        # RFC 8410: Ed448, like Ed25519, has no algorithm parameters.
        OpenSSL::ASN1::Sequence.new([
          OpenSSL::ASN1::Sequence.new([OpenSSL::ASN1::ObjectId.new('1.3.101.113')]),
          OpenSSL::ASN1::BitString.new(bytes)
        ]).to_der
      end
      private_class_method :ed448_subject_public_key

      def ed448_key
        unless @key.bytesize == 57
          raise OpenSSL::PKey::PKeyError, 'Ed448 DNSKEY must contain 57 octets'
        end
        @key_length = 448
        OpenSSL::PKey.read(DNSKEY.send(:ed448_subject_public_key, @key))
      end

      def rsa_key
        exponentLength = @key.getbyte(0)
        pos = 1
        if (exponentLength == 0)
          if @key.bytesize < 3
            raise OpenSSL::PKey::PKeyError, "Truncated RSA DNSKEY exponent length"
          end
          exponentLength = @key.byteslice(1, 2).unpack1('n')
          if exponentLength < 256
            raise OpenSSL::PKey::PKeyError, "Noncanonical RSA DNSKEY exponent length"
          end
          pos += 2
        end
        if !exponentLength || exponentLength == 0 || @key.bytesize <= pos + exponentLength
          raise OpenSSL::PKey::PKeyError, "Truncated RSA DNSKEY exponent or modulus"
        end
        # RFC 3110 section 2 limits both integers to 4096 bits and forbids
        # leading zero octets. Check bytes before constructing OpenSSL BNs.
        modulus_length = @key.bytesize - pos - exponentLength
        if exponentLength > 512 || modulus_length > 512 ||
            @key.getbyte(pos) == 0 || @key.getbyte(pos + exponentLength) == 0
          raise OpenSSL::PKey::PKeyError, "Invalid RSA DNSKEY integer encoding or size"
        end
        bits = (modulus_length - 1) * 8 + @key.getbyte(pos + exponentLength).bit_length
        # RFC 3110 section 3 and RFC 5702 section 2 define modulus minima.
        minimum = @algorithm == Algorithms.RSASHA512 ? 1024 : 512
        if bits < minimum
          raise OpenSSL::PKey::PKeyError, "RSA DNSKEY modulus must be at least #{minimum} bits"
        end
        exponent = RR::get_num(@key[pos, exponentLength])
        pos += exponentLength

        modulus = RR::get_num(@key[pos, @key.length])
        @key_length = bits

        data_sequence = OpenSSL::ASN1::Sequence([
                                                  OpenSSL::ASN1::Integer(modulus),
                                                  OpenSSL::ASN1::Integer(exponent)
                                                ])
        asn1 = OpenSSL::ASN1::Sequence(data_sequence)
        pkey = OpenSSL::PKey::RSA.new(asn1.to_der)
        return pkey
      end

      def dsa_key
        t = @key.getbyte(0)
        if !t || t > 8 || @key.bytesize != 21 + 3 * (t * 8 + 64)
          raise OpenSSL::PKey::PKeyError, "Invalid DSA DNSKEY length"
        end
        pgy_len = t * 8 + 64
        pos = 1
        q = RR::get_num(@key[pos, 20])
        pos += 20
        p = RR::get_num(@key[pos, pgy_len])
        pos += pgy_len
        g = RR::get_num(@key[pos, pgy_len])
        pos += pgy_len
        y = RR::get_num(@key[pos, pgy_len])
        pos += pgy_len
        @key_length = (pgy_len * 8)

        asn1 = OpenSSL::ASN1::Sequence.new(
          [
            OpenSSL::ASN1::Sequence.new(
              [
                OpenSSL::ASN1::ObjectId.new('DSA'),
                OpenSSL::ASN1::Sequence.new(
                  [
                    OpenSSL::ASN1::Integer.new(p),
                    OpenSSL::ASN1::Integer.new(q),
                    OpenSSL::ASN1::Integer.new(g)
                  ]
                )
              ]
            ),
            OpenSSL::ASN1::BitString.new(OpenSSL::ASN1::Integer.new(y).to_der)
          ]
        )

        OpenSSL::PKey::DSA.new(asn1.to_der)
      end

      # RFC6605, section 4
      # ECDSA public keys consist of a single value, called "Q" in FIPS
      # 186-3.  In DNSSEC keys, Q is a simple bit string that represents the
      # uncompressed form of a curve point, "x | y".
      def ec_key(curve = 'prime256v1')
        group = OpenSSL::PKey::EC::Group.new(curve)
        # DNSSEC pub does not have first octet that determines whether it's uncompressed
        # or compressed form, but it's required by OpenSSL to parse EC point correctly
        dnskey_bn = OpenSSL::BN.new("\x04" + @key, 2)
        key_point = OpenSSL::PKey::EC::Point.new(group, dnskey_bn)
        
        asn1 = OpenSSL::ASN1::Sequence.new(
          [
            OpenSSL::ASN1::Sequence.new([
              OpenSSL::ASN1::ObjectId.new("id-ecPublicKey"),
              OpenSSL::ASN1::ObjectId.new(group.curve_name)
            ]),
            OpenSSL::ASN1::BitString.new(key_point.to_octet_string(:uncompressed))
          ]
        )
        OpenSSL::PKey::EC.new(asn1.to_der)
      end
    end
  end
end
