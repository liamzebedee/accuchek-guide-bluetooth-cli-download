#!/usr/bin/env python3
import asyncio
from bleak import BleakClient

ADDR = "04:47:07:32:14:BD"

async def main():
    async with BleakClient(ADDR, timeout=30.0) as c:
        for s in c.services:
            print(f"SVC {s.uuid}  handle={s.handle}")
            for ch in s.characteristics:
                props = ",".join(ch.properties)
                print(f"  CHR {ch.uuid}  handle={ch.handle}  [{props}]")

asyncio.run(main())
