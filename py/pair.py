#!/usr/bin/env python3
"""Register a BlueZ Agent1 with fixed passkey and pair the meter."""

import sys
import dbus
import dbus.service
import dbus.mainloop.glib
from gi.repository import GLib

ADDR = "04:47:07:32:14:BD"
PASSKEY = 538425

BUS_NAME = "org.bluez"
AGENT_PATH = "/sugar/agent"
AGENT_IFACE = "org.bluez.Agent1"
ADAPTER_IFACE = "org.bluez.Adapter1"
DEVICE_IFACE = "org.bluez.Device1"
AGENT_MGR_IFACE = "org.bluez.AgentManager1"


class Agent(dbus.service.Object):
    @dbus.service.method(AGENT_IFACE, in_signature="", out_signature="")
    def Release(self):
        print("[agent] Release")

    @dbus.service.method(AGENT_IFACE, in_signature="os", out_signature="")
    def AuthorizeService(self, device, uuid):
        print(f"[agent] AuthorizeService {device} {uuid}")
        return

    @dbus.service.method(AGENT_IFACE, in_signature="o", out_signature="s")
    def RequestPinCode(self, device):
        print(f"[agent] RequestPinCode {device} -> {PASSKEY}")
        return str(PASSKEY)

    @dbus.service.method(AGENT_IFACE, in_signature="o", out_signature="u")
    def RequestPasskey(self, device):
        print(f"[agent] RequestPasskey {device} -> {PASSKEY}")
        return dbus.UInt32(PASSKEY)

    @dbus.service.method(AGENT_IFACE, in_signature="ouq", out_signature="")
    def DisplayPasskey(self, device, passkey, entered):
        print(f"[agent] DisplayPasskey {device} {passkey} entered={entered}")

    @dbus.service.method(AGENT_IFACE, in_signature="os", out_signature="")
    def DisplayPinCode(self, device, pincode):
        print(f"[agent] DisplayPinCode {device} {pincode}")

    @dbus.service.method(AGENT_IFACE, in_signature="ou", out_signature="")
    def RequestConfirmation(self, device, passkey):
        print(f"[agent] RequestConfirmation {device} {passkey}")
        return

    @dbus.service.method(AGENT_IFACE, in_signature="o", out_signature="")
    def RequestAuthorization(self, device):
        print(f"[agent] RequestAuthorization {device}")
        return

    @dbus.service.method(AGENT_IFACE, in_signature="", out_signature="")
    def Cancel(self):
        print("[agent] Cancel")


def find_adapter(bus):
    mgr = dbus.Interface(
        bus.get_object(BUS_NAME, "/"),
        "org.freedesktop.DBus.ObjectManager",
    )
    for path, ifaces in mgr.GetManagedObjects().items():
        if ADAPTER_IFACE in ifaces:
            return path
    raise RuntimeError("no BlueZ adapter")


def find_device(bus, addr):
    mgr = dbus.Interface(
        bus.get_object(BUS_NAME, "/"),
        "org.freedesktop.DBus.ObjectManager",
    )
    for path, ifaces in mgr.GetManagedObjects().items():
        dev = ifaces.get(DEVICE_IFACE)
        if dev and dev.get("Address", "").upper() == addr.upper():
            return path
    return None


def main():
    dbus.mainloop.glib.DBusGMainLoop(set_as_default=True)
    bus = dbus.SystemBus()

    Agent(bus, AGENT_PATH)
    mgr = dbus.Interface(
        bus.get_object(BUS_NAME, "/org/bluez"),
        AGENT_MGR_IFACE,
    )
    mgr.RegisterAgent(AGENT_PATH, "KeyboardOnly")
    mgr.RequestDefaultAgent(AGENT_PATH)
    print("agent registered + default")

    adapter_path = find_adapter(bus)
    adapter = dbus.Interface(
        bus.get_object(BUS_NAME, adapter_path), ADAPTER_IFACE
    )
    adapter_props = dbus.Interface(
        bus.get_object(BUS_NAME, adapter_path),
        "org.freedesktop.DBus.Properties",
    )
    adapter_props.Set(ADAPTER_IFACE, "Powered", True)

    # Make sure we have the device. If not, scan until we find it (up to 60s).
    dev_path = find_device(bus, ADDR)
    if not dev_path:
        print("device not cached, starting discovery (up to 60s)…")
        print("  >>> put the meter in pairing mode now (hold both arrows ~3s) <<<")
        try:
            adapter.StartDiscovery()
        except dbus.exceptions.DBusException as e:
            print(f"  (discovery start: {e})")

        discovery_loop = GLib.MainLoop()
        state = {"path": None}

        def poll():
            p = find_device(bus, ADDR)
            if p:
                state["path"] = p
                discovery_loop.quit()
                return False
            return True

        GLib.timeout_add(500, poll)
        GLib.timeout_add(60_000, lambda: (discovery_loop.quit(), False)[1])
        discovery_loop.run()

        try:
            adapter.StopDiscovery()
        except Exception:
            pass
        dev_path = state["path"]
        if not dev_path:
            print("could not find device — meter not advertising")
            sys.exit(2)

    print(f"device path: {dev_path}")
    device = dbus.Interface(
        bus.get_object(BUS_NAME, dev_path), DEVICE_IFACE
    )

    # Pair
    loop = GLib.MainLoop()

    def paired_ok():
        print("PAIRED OK")
        try:
            device.Set = None  # noqa
        except Exception:
            pass
        # Trust + connect
        props = dbus.Interface(
            bus.get_object(BUS_NAME, dev_path),
            "org.freedesktop.DBus.Properties",
        )
        props.Set(DEVICE_IFACE, "Trusted", True)
        print("trusted.")
        loop.quit()

    def paired_err(err):
        print(f"PAIR FAILED: {err}")
        loop.quit()

    print("calling Pair()…")
    device.Pair(reply_handler=paired_ok, error_handler=paired_err, timeout=60)
    loop.run()


if __name__ == "__main__":
    main()
