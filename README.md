# JetKVM Ethernet-over-USB (CDC-NCM Passthrough)
This is absolutely ALL made by LLM. I just uploaded this "manual" so there is no need to burn tokens recreating this (if anyone needs this at all except me).


Enable host network access and internet passthrough over the single USB-C cable on your [JetKVM](https://jetkvm.com).

This tool adds a **CDC-NCM** Ethernet function to the JetKVM USB gadget, starts an embedded lightweight DHCP server on the device, and configures `nftables` NAT so the connected computer gets network and internet connectivity through the JetKVM.

---

## ⚡ Quick Install (One Command)

SSH into your JetKVM device as `root` and run:

```bash
curl -sSL https://raw.githubusercontent.com/cue4u/jetkvm-ethernet-over-usb/main/install.sh | sh
```

Or using `wget`:
```bash
wget -qO- https://raw.githubusercontent.com/cue4u/jetkvm-ethernet-over-usb/main/install.sh | sh
```

That's it! Your host PC will instantly detect the network adapter and receive IP `192.168.42.2` with full internet access through JetKVM.

---

## ⚠️ Critical Hardware Warnings & Advisory

Before using or benchmarking this feature, please understand the physical hardware constraints:

### 1. Hardware Limit: 100 Mbit/s Max (No Gigabit)
- The JetKVM is built around the **Rockchip RV1106** SoC.
- The onboard physical Ethernet port (`eth0`) is connected via an embedded RMII PHY (`RK630 PHY`) that only supports **10/100 Mbps Full Duplex**. It physically does not have Gigabit hardware.
- The USB-C port operates in USB 2.0 High-Speed mode (480 Mbps theoretical).
- Therefore, your maximum real-world throughput over USB Ethernet will be approximately **100 Mbps**.

### 2. The Windows RNDIS / Code 10 Trap
- **CDC-ECM** does not work out-of-the-box on Windows 10/11 because Microsoft never shipped an inbox CDC-ECM driver for composite USB devices.
- **RNDIS** with Microsoft OS Feature Descriptors (`MSFT100`) often causes Windows to load the legacy 2006 driver (`wceisvista.inf` / `rndismpx.sys`), resulting in:
  ```
  Remote NDIS based Internet Sharing Device - This device cannot start (Code 10)
  ```
- **Why CDC-NCM is used here:** Windows 10 (version 1607+) and Windows 11 include the native **`UsbNcm.sys`** driver. macOS and Linux support CDC-NCM out-of-the-box without any configuration. It provides a true zero-configuration experience across all operating systems.

### 3. Safe from Firmware Overwrite
- All files live exclusively on `/userdata` (`/userdata/bin` and `/userdata/init.d/`).
- The stock root partitions remain untouched (zero brick risk).
- Boot persistence is handled cleanly by `/userdata/init.d/S99usb_ethernet.sh`, which is automatically invoked by the system launcher (`/oem/usr/bin/RkLunch.sh`).

---

## Architecture & How It Works

```
 [ Target Host Computer ]
            │  USB-C Cable (CDC-NCM)
            ▼
    [ JetKVM usb0 / usb2 ]  (IP: 192.168.42.1/24)
            │
      jetkvm-dhcpd (Embedded DHCP server assigns 192.168.42.2 to Host)
            │
      nftables NAT & IP Forwarding (/proc/sys/net/ipv4/ip_forward = 1)
            │
    [ JetKVM eth0 ]         (LAN IP, e.g. 192.168.75.x)
            │
            ▼
    [ Local LAN & Internet (Up to 100 Mbps) ]
```

---

## Management & Commands

SSH into your JetKVM to control or check the service:

- **Check status:**
  ```bash
  /userdata/bin/usb-ethernet-ctl status
  ```
- **Stop USB Ethernet:**
  ```bash
  /userdata/bin/usb-ethernet-ctl stop
  ```
- **Start USB Ethernet:**
  ```bash
  /userdata/bin/usb-ethernet-ctl start
  ```
- **Restart service:**
  ```bash
  /userdata/bin/usb-ethernet-ctl restart
  ```
- **View live DHCP logs:**
  ```bash
  cat /tmp/jetkvm-dhcpd.log
  ```

---

## Repository Files

- **`install.sh`**: Downloadable one-line automated installer.
- **`usb-ethernet-ctl`**: Production lifecycle script for the gadget, routing, and DHCP.
- **`dhcpd.go`**: Go source code for the raw `AF_PACKET` DHCP server for ARMv7.
- **`releases/`**: Contains precompiled static ARMv7 `jetkvm-dhcpd` binary.

---

## License

MIT License.
