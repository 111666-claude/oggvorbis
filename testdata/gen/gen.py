#!/usr/bin/env python3
# Regenerate the Vorbis fixtures used by the loop-point tests.
# Run from the repository root:  python3 -I testdata/gen/gen.py
#
# Requires ffmpeg with libvorbis. Re-running the script overwrites the
# loop_*.ogg files in testdata/. ffmpeg is not byte-for-byte deterministic
# (page serial numbers and muxing timestamps vary between runs), but the
# decoded PCM and comment headers are stable, which is what the Go tests
# check against.
#
# ffmpeg/libvorbis deduplicates comment keys, so the fixture containing two
# LOOPSTART comments is produced by re-muxing an encoded file with a custom
# comment packet (and recomputing Ogg page checksums).
import os
import struct
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.dirname(HERE)

CRC_POLY = 0x04C11DB7
CRC_TABLE = []
for _i in range(256):
    _r = _i << 24
    for _ in range(8):
        _r = ((_r << 1) ^ CRC_POLY) & 0xFFFFFFFF if _r & 0x80000000 else (_r << 1) & 0xFFFFFFFF
    CRC_TABLE.append(_r)


def crc32ogg(data):
    crc = 0
    for b in data:
        crc = ((crc << 8) & 0xFFFFFFFF) ^ CRC_TABLE[((crc >> 24) & 0xFF) ^ b]
    return crc


def parse_pages(path):
    data = open(path, "rb").read()
    pos = 0
    pages = []
    while pos + 27 <= len(data) and data[pos:pos + 4] == b"OggS":
        version, flags = data[pos + 4], data[pos + 5]
        gp, serial, seq, checksum = struct.unpack_from("<qIIi", data, pos + 6)
        nseg = data[pos + 26]
        segs = list(data[pos + 27:pos + 27 + nseg])
        body = data[pos + 27 + nseg:pos + 27 + nseg + sum(segs)]
        pages.append({"flags": flags, "gp": gp, "serial": serial,
                      "segs": segs, "body": body})
        pos += 27 + nseg + sum(segs)
    assert pos == len(data), "trailing bytes in %s" % path
    return pages


def split_packets(pages):
    """Return (packets, completion) where completion[i] is the granule
    position of the page on which packet i ends."""
    packets, completion = [], []
    cur = bytearray()
    for page in pages:
        off = 0
        for seg in page["segs"]:
            cur += page["body"][off:off + seg]
            off += seg
            if seg < 255:
                packets.append(bytes(cur))
                completion.append(page["gp"])
                cur = bytearray()
    assert not cur
    return packets, completion


def build_page(flags, gp, serial, seq, body, segs):
    header = bytearray(27)
    header[0:4] = b"OggS"
    header[4] = 0
    header[5] = flags
    struct.pack_into("<qIIiB", header, 6, gp, serial, seq, 0, len(segs))
    header[22:26] = b"\x00\x00\x00\x00"
    page = bytes(header) + bytes(segs) + bytes(body)
    checksum = crc32ogg(page)
    struct.pack_into("<I", header, 22, checksum)
    return bytes(header) + bytes(segs) + bytes(body)


def segment_packet(packet):
    segs = [255] * (len(packet) // 255)
    rem = len(packet) % 255
    if rem or not segs:
        segs.append(rem)
    return segs


def mux(packets, completion, serial):
    """Simple single-stream Ogg muxer: every source page becomes one output
    page carrying the packets that completed on it (plus any partial packet
    carried in from the previous page)."""
    pages = []
    seq = 0
    pending = bytearray()          # bytes of a packet that spans pages
    i = 0
    # group packets by their completion granule position
    groups = []
    for pkt, gp in zip(packets, completion):
        if groups and groups[-1][0] == gp:
            groups[-1][1].append(pkt)
        else:
            groups.append((gp, [pkt]))
    for gi, (gp, group) in enumerate(groups):
        body = bytearray(pending)
        segs = []
        if pending:
            segs.append(255) if len(pending) % 255 == 0 else None
        first_flags = 1 if pending else 0
        pending = bytearray()
        for pi, pkt in enumerate(group):
            raw = pkt
            if pi < len(group) - 1 or gp != 0 or gi == len(groups) - 1:
                pass
            psegs = segment_packet(raw)
            segs.extend(psegs)
            body += raw
        if len(segs) > 255:
            raise AssertionError("packet burst does not fit one page")
        flags = first_flags
        if seq == 0:
            flags |= 2
        if gi == len(groups) - 1:
            flags |= 4
        pages.append(build_page(flags, gp, serial, seq, body, segs))
        seq += 1
    return b"".join(pages)


def make_comment_packet(vendor, comments):
    p = b"\x03vorbis"
    p += struct.pack("<I", len(vendor)) + vendor.encode()
    p += struct.pack("<I", len(comments))
    for c in comments:
        b = c.encode()
        p += struct.pack("<I", len(b)) + b
    return p


def encode(src_expr, dst, extra_meta, duration, channels):
    cmd = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
           "-f", "lavfi", "-i",
           "aevalsrc=%s:s=44100:d=%g" % (src_expr, duration),
           "-c:a", "libvorbis", "-b:a", "96k"]
    for k, v in extra_meta:
        cmd += ["-metadata", "%s=%s" % (k, v)]
    cmd.append(dst)
    subprocess.run(cmd, check=True)


def main():
    stereo = ("0.6*sin(2*PI*440*t)+0.25*sin(2*PI*1500*t)|"
              "0.6*cos(2*PI*440*t)-0.25*sin(2*PI*1500*t)")
    mono = "0.6*sin(2*PI*440*t)+0.25*sin(2*PI*1500*t)"

    jobs = [
        # (name, expression, duration, metadata)
        ("loop.ogg", stereo, 1.0,
         [("LOOPSTART", "10000"), ("LOOPEND", "40000")]),
        ("loop_mono.ogg", mono, 0.5,
         [("LOOPSTART", "8000"), ("LOOPEND", "16000")]),
        ("loop_notags.ogg", stereo, 1.0, []),
        ("loop_inverted.ogg", stereo, 1.0,
         [("LOOPSTART", "40000"), ("LOOPEND", "10000")]),
        ("loop_outofrange.ogg", stereo, 1.0,
         [("LOOPSTART", "10000"), ("LOOPEND", "999999")]),
        ("loop_badvalue.ogg", stereo, 1.0,
         [("LOOPSTART", "abc"), ("LOOPEND", "40000")]),
    ]
    for name, expr, dur, meta in jobs:
        encode(expr, os.path.join(OUT, name), meta, dur,
               2 if "|" in expr else 1)

    # duplicate LOOPSTART: re-mux loop.ogg with a custom comment packet
    src = os.path.join(OUT, "loop.ogg")
    pages = parse_pages(src)
    packets, completion = split_packets(pages)
    assert packets[1][:7] == b"\x03vorbis"
    packets[1] = make_comment_packet(
        "loop-test", ["LOOPSTART=10000", "LOOPSTART=11111", "LOOPEND=40000"])
    serial = pages[0]["serial"]
    with open(os.path.join(OUT, "loop_dupstart.ogg"), "wb") as f:
        f.write(mux(packets, completion, serial))
    print("generated fixtures in", OUT)


if __name__ == "__main__":
    sys.exit(main())
