#!/usr/bin/env python3
"""Connect to paired meter, read all glucose records via RACP, write TSV."""

import asyncio
import os
import struct
import sys
from datetime import datetime

from bleak import BleakClient

ADDR = "04:47:07:32:14:BD"
TSV = "/home/liam/Documents/projects/diabetes/sugar/records.tsv"

# Handles from the meter's GATT tree (Glucose Service 0x1808):
GM_HANDLE = 7    # Glucose Measurement (notify)
RACP_HANDLE = 12 # Record Access Control Point (write+indicate)


def sfloat(raw: int) -> float:
    m = raw & 0x0FFF
    e = (raw >> 12) & 0x0F
    if m >= 0x0800: m -= 0x1000
    if e >= 0x08:   e -= 0x10
    return m * (10 ** e)


def parse_gm(data: bytes) -> dict:
    flags = data[0]
    seq = struct.unpack_from("<H", data, 1)[0]
    y, mo, d, h, mi, s = struct.unpack_from("<HBBBBB", data, 3)
    off = 10
    ts = None
    if y:
        try: ts = datetime(y, mo, d, h, mi, s)
        except ValueError: ts = None
    if flags & 0x01:
        off += 2  # time offset
    conc = None
    unit = None
    if flags & 0x02:
        raw = struct.unpack_from("<H", data, off)[0]
        conc = sfloat(raw)
        off += 1 + 2  # skip type/location byte too
        unit = "mol/L" if (flags & 0x04) else "kg/L"
    mmol = mgdl = None
    if conc is not None:
        if unit == "mol/L":
            mmol = conc * 1000.0
            mgdl = mmol * 18.0182
        else:
            mgdl = conc * 1e5
            mmol = mgdl / 18.0182
    return {"seq": seq, "ts": ts, "mmol": mmol, "mgdl": mgdl, "raw": data.hex()}


def load_last_seq(path: str) -> int:
    if not os.path.exists(path):
        return -1
    mx = -1
    with open(path) as f:
        for i, line in enumerate(f):
            if i == 0 and line.startswith("sequence\t"):
                continue
            parts = line.split("\t", 1)
            try:
                n = int(parts[0])
                if n > mx: mx = n
            except ValueError:
                pass
    return mx


def open_tsv(path: str):
    new = not os.path.exists(path)
    f = open(path, "a")
    if new:
        f.write("sequence\ttimestamp\tmmol_l\tmg_dl\traw_hex\n")
        f.flush()
    return f


async def main():
    last = load_last_seq(TSV)
    print(f"last known seq: {last}")
    tsv = open_tsv(TSV)

    done = asyncio.Event()
    records = 0

    def on_gm(_, data: bytearray):
        nonlocal records
        r = parse_gm(bytes(data))
        records += 1
        ts = r["ts"].isoformat(sep=" ") if r["ts"] else ""
        mm = f"{r['mmol']:.2f}" if r["mmol"] is not None else ""
        md = f"{r['mgdl']:.1f}" if r["mgdl"] is not None else ""
        print(f"  #{r['seq']:>4} {ts}  {mm} mmol/L  ({md} mg/dL)")
        tsv.write(f"{r['seq']}\t{ts}\t{mm}\t{md}\t{r['raw']}\n")
        tsv.flush()

    def on_racp(_, data: bytearray):
        d = bytes(data)
        print(f"  RACP <- {d.hex()}")
        if len(d) >= 3 and d[0] == 0x06:
            print(f"  RACP done: req_op={d[1]} code={d[2]}")
            done.set()
        elif len(d) >= 3 and d[0] == 0x05:
            n = struct.unpack_from("<H", d, 1)[0]
            print(f"  RACP count: {n}")

    print(f"connecting {ADDR}…")
    async with BleakClient(ADDR, timeout=30.0) as c:
        print("connected.")

        gm_char = c.services.get_characteristic(GM_HANDLE)
        racp_char = c.services.get_characteristic(RACP_HANDLE)
        if gm_char is None or racp_char is None:
            print(f"  GM handle={gm_char}  RACP handle={racp_char}")
            sys.exit(2)

        await c.start_notify(gm_char, on_gm)
        await c.start_notify(racp_char, on_racp)

        if last < 0:
            cmd = bytes([0x01, 0x01])
            print("RACP: report all records")
        else:
            lo = (last + 1) & 0xFF
            hi = ((last + 1) >> 8) & 0xFF
            cmd = bytes([0x01, 0x03, 0x01, lo, hi])
            print(f"RACP: report records seq >= {last+1}")

        await c.write_gatt_char(racp_char, cmd, response=True)
        try:
            await asyncio.wait_for(done.wait(), timeout=120.0)
        except asyncio.TimeoutError:
            print("RACP timeout")

        await c.stop_notify(gm_char)
        await c.stop_notify(racp_char)

    tsv.close()
    print(f"\n{records} new records")


if __name__ == "__main__":
    asyncio.run(main())
