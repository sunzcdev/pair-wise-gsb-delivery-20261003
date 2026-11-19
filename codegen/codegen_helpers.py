# -*- coding: utf-8 -*-

from __future__ import print_function

def genSingleEncode(spec, cValue, unresolved_domain):
    buffer = []
    type = spec.resolveDomain(unresolved_domain)
    if type == 'shortstr':
        buffer.append("buffer << %s.to_s.bytesize.chr" % (cValue,))
        buffer.append("buffer << %s.to_s" % (cValue,))
    elif type == 'longstr':
        buffer.append("buffer << [%s.to_s.bytesize].pack(PACK_UINT32)" % (cValue,))
        buffer.append("buffer << %s.to_s" % (cValue,))
    elif type == 'octet':
        buffer.append("buffer << [%s].pack(PACK_CHAR)" % (cValue,))
    elif type == 'short':
        buffer.append("buffer << [%s].pack(PACK_UINT16)" % (cValue,))
    elif type == 'long':
        buffer.append("buffer << [%s].pack(PACK_UINT32)" % (cValue,))
    elif type == 'longlong':
        buffer.append("buffer << AMQ::Pack.pack_uint64_big_endian(%s)" % (cValue,))
    elif type == 'timestamp':
        buffer.append("buffer << AMQ::Pack.pack_uint64_big_endian(%s)" % (cValue,))
    elif type == 'bit':
        raise ValueError("Can't encode bit in genSingleEncode")
    elif type == 'table':
        buffer.append("buffer << AMQ::Protocol::Table.encode(%s)" % (cValue,))
    else:
        raise ValueError("Illegal domain in genSingleEncode: {0}".format(type))

    return buffer

def genSingleDecode(spec, field):
    cLvalue = field.ruby_name
    unresolved_domain = field.domain

    type = spec.resolveDomain(unresolved_domain)
    buffer = []
    if type == 'shortstr':
        buffer.append("length = data.getbyte(offset)")
        buffer.append("offset += 1")
        buffer.append("%s = data.byteslice(offset, length)" % (cLvalue,))
        buffer.append("offset += length")
    elif type == 'longstr':
        buffer.append("length = data.byteslice(offset, 4).unpack1(PACK_UINT32)")
        buffer.append("offset += 4")
        buffer.append("%s = data.byteslice(offset, length)" % (cLvalue,))
        buffer.append("offset += length")
    elif type == 'octet':
        buffer.append("%s = data.getbyte(offset)" % (cLvalue,))
        buffer.append("offset += 1")
    elif type == 'short':
        buffer.append("%s = data.byteslice(offset, 2).unpack1(PACK_UINT16)" % (cLvalue,))
        buffer.append("offset += 2")
    elif type == 'long':
        buffer.append("%s = data.byteslice(offset, 4).unpack1(PACK_UINT32)" % (cLvalue,))
        buffer.append("offset += 4")
    elif type == 'longlong':
        buffer.append("%s = data.byteslice(offset, 8).unpack1(PACK_UINT64_BE)" % (cLvalue,))
        buffer.append("offset += 8")
    elif type == 'timestamp':
        buffer.append("%s = Time.at(data.byteslice(offset, 8).unpack1(PACK_UINT64_BE))" % (cLvalue,))
        buffer.append("offset += 8")
    elif type == 'bit':
        raise ValueError("Can't decode bit in genSingleDecode")
    elif type == 'table':
        buffer.append("table_length = Table.length(data.byteslice(offset, 4))")
        buffer.append("%s = Table.decode(data.byteslice(offset, table_length + 4))" % (cLvalue,))
        buffer.append("offset += table_length + 4")
    else:
        raise ValueError("Illegal domain '{0}' in genSingleDecode".format(type))

    return buffer



def genSingleSimpleDecode(spec, field):
    cLvalue = field.ruby_name
    unresolved_domain = field.domain

    type = spec.resolveDomain(unresolved_domain)
    buffer = []
    if type == 'shortstr':
        buffer.append("data.to_s")
    elif type == 'longstr':
        buffer.append("data.to_s")
    elif type == 'octet':
        buffer.append("data.unpack(PACK_INT8).first")
    elif type == 'short':
        buffer.append("data.unpack(PACK_UINT16).first")
    elif type == 'long':
        buffer.append("data.unpack(PACK_UINT32).first")
    elif type == 'longlong':
        buffer.append("AMQ::Pack.unpack_uint64_big_endian(data).first")
    elif type == 'timestamp':
        buffer.append("Time.at(data.unpack(PACK_UINT64_BE).last)")
    elif type == 'bit':
        raise ValueError("Can't decode bit in genSingleDecode")
    elif type == 'table':
        buffer.append("Table.decode(data)")
    else:
        raise ValueError("Illegal domain '" + type + "' in genSingleSimpleDecode")

    return buffer


def genEncodeMethodDefinition(spec, m):
    # Adjacent bit arguments share one byte. A non-bit field always flushes
    # the current bit group, so a later group starts at bit offset 0 of a new
    # byte instead of reusing the previous group's offset.
    def flush_bits():
        if bit_index is not None:
            buffer.append("buffer << [bit_buffer].pack(PACK_CHAR)")

    bit_index = None
    buffer = []

    for f in m.arguments:
        if spec.resolveDomain(f.domain) == 'bit':
            if bit_index is None:
                bit_index = 0
                buffer.append("bit_buffer = 0")
            elif bit_index >= 8:
                flush_bits()
                buffer.append("bit_buffer = 0")
                bit_index = 0
            # Inline branch keeps the legacy truthiness fast path free of
            # method calls; strict mode only accepts real booleans and nil
            # (an omitted optional bit).
            buffer.append("if StrictParameters.enabled? && !%s.nil? && !%s.equal?(true) && !%s.equal?(false)" % (f.ruby_name, f.ruby_name, f.ruby_name))
            buffer.append("  raise InvalidParameterError.new(@name, :%s, \"true, false or nil\", %s)" % (f.ruby_name, f.ruby_name))
            buffer.append("end")
            buffer.append("bit_buffer |= ((%s ? 1 : 0) << %d)" % (f.ruby_name, bit_index))
            bit_index = bit_index + 1
        else:
            flush_bits()
            bit_index = None
            buffer += genSingleEncode(spec, f.ruby_name, f.domain)

    flush_bits()
    return buffer

def genDecodeMethodDefinition(spec, m):
    # Every bit group occupies its own byte, even when fewer than 8 bits are
    # used. A non-bit field between two bit groups forces a fresh byte read.
    buffer = []
    bit_index = None
    for f in m.arguments:
        if spec.resolveDomain(f.domain) == 'bit':
            if bit_index is None:
                bit_index = 0
                buffer.append("bit_buffer = data.getbyte(offset)")
                buffer.append("offset += 1")
            elif bit_index >= 8:
                buffer.append("bit_buffer = data.getbyte(offset)")
                buffer.append("offset += 1")
                bit_index = 0
            buffer.append("%s = (bit_buffer & (1 << %d)) != 0" % (f.ruby_name, bit_index))
            bit_index = bit_index + 1
        else:
            bit_index = None
            buffer += genSingleDecode(spec, f)
    return buffer
