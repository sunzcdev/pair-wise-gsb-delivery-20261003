#!/usr/bin/env python3
"""Generate reference xz files for the Delta/x86-BCJ filter tests.

The files in this directory consumed by filter_test.go are produced
with this script. Python's lzma module wraps the reference C
implementation liblzma (https://tukaani.org/xz/), so the streams
verify wire-level interoperability with an independent, authoritative
implementation.

Re-run with:  python3 testdata/gen_refs.py
Requires a Python build whose lzma module provides FILTER_DELTA and
FILTER_X86 (Python 3.3+ with liblzma 5.0+).
"""
import lzma
import os
import random

HERE = os.path.dirname(os.path.abspath(__file__))

FOX = b"The quick brown fox jumps over the lazy dog.\n"


def write(name, body, filters, check=lzma.CHECK_CRC64):
    data = lzma.compress(body, format=lzma.FORMAT_XZ, check=check,
                         filters=filters)
    with open(os.path.join(HERE, name), "wb") as f:
        f.write(data)


def code_body():
    """5000 bytes with E8/E9 instructions at arbitrary positions."""
    rnd = random.Random(1234)
    body = bytearray()

    def add(b):
        if len(body) < 5000:
            body.append(b)

    while len(body) < 5000:
        if len(body) % 3 == 0:
            add(rnd.choice((0xE8, 0xE9)))
            for b in rnd.randrange(1 << 32).to_bytes(4, "little"):
                add(b)
        else:
            for _ in range(rnd.randrange(1, 9)):
                add(rnd.randrange(256))
    return bytes(body)


def main():
    lz6 = {"id": lzma.FILTER_LZMA2, "preset": 6,
           "dict_size": 8 * 1024 * 1024}
    delta = lambda d: {"id": lzma.FILTER_DELTA, "dist": d}
    x86 = lambda off=0: {"id": lzma.FILTER_X86, "start_offset": off}

    write("fox-delta.xz", FOX, [delta(1), lz6])
    write("fox-x86.xz", FOX, [x86(), lz6])
    write("fox-delta-x86.xz", FOX, [delta(1), x86(), lz6])
    write("fox-x86-off.xz", FOX, [x86(0x1234), lz6])

    body = code_body()
    with open(os.path.join(HERE, "code.bin"), "wb") as f:
        f.write(body)
    write("code-lzma2.xz", body, [lz6])
    write("code-delta.xz", body, [delta(1), lz6])
    write("code-delta4.xz", body, [delta(4), lz6])
    write("code-delta7.xz", body, [delta(7), lz6])
    write("code-x86.xz", body, [x86(), lz6])
    write("code-x86-off.xz", body, [x86(0xABC), lz6])
    write("code-delta-x86.xz", body, [delta(1), x86(), lz6])


if __name__ == "__main__":
    main()
