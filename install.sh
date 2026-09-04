#!/bin/sh
# install.sh
# One-line installer for JetKVM Ethernet-over-USB (CDC-NCM + NAT + DHCP)
set -e

echo "=================================================="
echo "    JetKVM Ethernet-over-USB Automatic Installer  "
echo "=================================================="

# 1. Check environment
if [ ! -d "/sys/kernel/config/usb_gadget/jetkvm" ]; then
    echo "ERROR: JetKVM USB gadget configfs directory not found."
    echo "Make sure this script is running directly on the JetKVM device as root via SSH."
    exit 1
fi

mkdir -p /userdata/bin
mkdir -p /userdata/init.d

# 2. Download pre-compiled ARMv7 DHCP server binary
DHCP_BIN="/userdata/bin/jetkvm-dhcpd"
echo "[1/4] Downloading pre-compiled ARMv7 jetkvm-dhcpd binary..."
DHCP_URL="https://github.com/cue4u/jetkvm-ethernet-over-usb/releases/download/v1.0.0/jetkvm-dhcpd"

if command -v curl >/dev/null 2>&1; then
    curl -sSL -o "$DHCP_BIN" "$DHCP_URL"
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$DHCP_BIN" "$DHCP_URL"
else
    echo "ERROR: Neither curl nor wget found!"
    exit 1
fi

chmod 755 "$DHCP_BIN"

# 3. Install management script /userdata/bin/usb-ethernet-ctl
echo "[2/4] Installing /userdata/bin/usb-ethernet-ctl..."
cat << 'EOF' > /userdata/bin/usb-ethernet-ctl
#!/bin/sh
# /userdata/bin/usb-ethernet-ctl
# Controls USB Ethernet (CDC-NCM) gadget and NAT networking on JetKVM

GADGET_DIR="/sys/kernel/config/usb_gadget/jetkvm"
UDC="ffb00000.usb"
SERVER_IP="192.168.42.1"
CLIENT_IP="192.168.42.2"
NETMASK="255.255.255.0"
DHCP_BIN="/userdata/bin/jetkvm-dhcpd"
PID_FILE="/tmp/jetkvm-dhcpd.pid"

start() {
    echo "[usb-ethernet] Starting USB Ethernet (CDC-NCM)..."

    # Wait for JetKVM gadget directory
    for i in $(seq 1 30); do
        [ -d "$GADGET_DIR" ] && break
        sleep 1
    done

    if [ ! -d "$GADGET_DIR" ]; then
        echo "[usb-ethernet] ERROR: Gadget directory $GADGET_DIR not found!"
        return 1
    fi

    # Configure IAD class codes for composite multi-function device
    echo 0xEF > "$GADGET_DIR/bDeviceClass"
    echo 0x02 > "$GADGET_DIR/bDeviceSubClass"
    echo 0x01 > "$GADGET_DIR/bDeviceProtocol"

    # Create NCM function if not present
    if [ ! -d "$GADGET_DIR/functions/ncm.usb0" ]; then
        mkdir -p "$GADGET_DIR/functions/ncm.usb0"
    fi

    # Disable OS desc so Windows binds UsbNcm.sys rather than failing on RNDIS Code 10
    if [ -d "$GADGET_DIR/os_desc" ]; then
        echo 0 > "$GADGET_DIR/os_desc/use" 2>/dev/null || true
        rm -f "$GADGET_DIR/os_desc/c.1" 2>/dev/null || true
    fi

    # Link function to configs/c.1 if not present
    if [ ! -L "$GADGET_DIR/configs/c.1/ncm.usb0" ]; then
        echo "[usb-ethernet] Linking ncm.usb0 to config c.1..."
        echo "" > "$GADGET_DIR/UDC"
        rm -f "$GADGET_DIR/configs/c.1/rndis.usb0" 2>/dev/null
        rm -f "$GADGET_DIR/configs/c.1/ecm.usb0" 2>/dev/null
        ln -sf "$GADGET_DIR/functions/ncm.usb0" "$GADGET_DIR/configs/c.1/"
        echo "$UDC" > "$GADGET_DIR/UDC"
    fi

    # Detect generated interface name
    IFACE=""
    if [ -f "$GADGET_DIR/functions/ncm.usb0/ifname" ]; then
        IFACE=$(cat "$GADGET_DIR/functions/ncm.usb0/ifname")
    fi

    if [ -z "$IFACE" ] || [ "$IFACE" = "(unnamed net_device)" ]; then
        for i in $(seq 1 10); do
            for candidate in usb2 usb1 usb0; do
                if [ -e "/sys/class/net/$candidate" ]; then
                    IFACE="$candidate"
                    break 2
                fi
            done
            sleep 1
        done
    fi

    if [ -z "$IFACE" ]; then
        echo "[usb-ethernet] ERROR: USB net device did not appear!"
        return 1
    fi

    echo "[usb-ethernet] Configuring interface $IFACE..."
    ip link set "$IFACE" up
    if ! ip addr show dev "$IFACE" | grep -q "$SERVER_IP"; then
        ip addr add "${SERVER_IP}/24" dev "$IFACE"
    fi

    # Enable IP Forwarding
    echo 1 > /proc/sys/net/ipv4/ip_forward

    # Configure NAT via nftables
    modprobe nf_tables 2>/dev/null
    modprobe nft_nat 2>/dev/null
    modprobe nft_masq 2>/dev/null
    modprobe nft_chain_nat 2>/dev/null

    nft add table ip nat 2>/dev/null
    nft add chain ip nat postrouting '{ type nat hook postrouting priority srcnat ; }' 2>/dev/null
    if ! nft list table ip nat 2>/dev/null | grep -q 'oifname "eth0" masquerade'; then
        nft add rule ip nat postrouting oifname "eth0" masquerade 2>/dev/null
    fi

    # Start DHCP daemon bound to interface
    if [ -x "$DHCP_BIN" ]; then
        if [ -f "$PID_FILE" ] && kill -0 $(cat "$PID_FILE") 2>/dev/null; then
            echo "[usb-ethernet] DHCP server already running."
        else
            echo "[usb-ethernet] Starting DHCP server on $IFACE ($SERVER_IP -> $CLIENT_IP)..."
            $DHCP_BIN -iface "$IFACE" -ip "$SERVER_IP" -client-ip "$CLIENT_IP" -mask "$NETMASK" > /tmp/jetkvm-dhcpd.log 2>&1 &
            echo $! > "$PID_FILE"
        fi
    fi

    echo "[usb-ethernet] USB Ethernet active on $IFACE ($SERVER_IP/24)."
}

stop() {
    echo "[usb-ethernet] Stopping USB Ethernet..."
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        kill "$PID" 2>/dev/null
        rm -f "$PID_FILE"
    fi

    if [ -L "$GADGET_DIR/configs/c.1/ncm.usb0" ]; then
        echo "" > "$GADGET_DIR/UDC"
        rm -f "$GADGET_DIR/configs/c.1/ncm.usb0"
        echo "$UDC" > "$GADGET_DIR/UDC"
    fi

    for ifc in usb2 usb1 usb0; do
        ip link set "$ifc" down 2>/dev/null
    done
    echo "[usb-ethernet] Stopped."
}

status() {
    echo "=== USB Gadget Config ==="
    ls -l "$GADGET_DIR/configs/c.1/"
    echo "=== Interface ==="
    for ifc in usb2 usb1 usb0; do
        if [ -e "/sys/class/net/$ifc" ]; then
            ip addr show dev "$ifc"
        fi
    done
    echo "=== IP Forwarding ==="
    cat /proc/sys/net/ipv4/ip_forward
    echo "=== NAT Rules ==="
    nft list table ip nat 2>/dev/null || echo "No NAT table"
    echo "=== DHCP Daemon ==="
    if [ -f "$PID_FILE" ] && kill -0 $(cat "$PID_FILE") 2>/dev/null; then
        echo "Running (PID $(cat $PID_FILE))"
    else
        echo "Not running"
    fi
    echo "=== Neighbor Table ==="
    ip neigh show
}

case "$1" in
    start)
        start
        ;;
    stop)
        stop
        ;;
    restart)
        stop
        sleep 1
        start
        ;;
    status)
        status
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac
EOF
chmod 755 /userdata/bin/usb-ethernet-ctl

# 4. Install boot persistence hook
echo "[3/4] Enabling boot persistence in /userdata/init.d/S99usb_ethernet.sh..."
cat << 'EOF' > /userdata/init.d/S99usb_ethernet.sh
#!/bin/sh
case "$1" in
    start)
        /userdata/bin/usb-ethernet-ctl start &
        ;;
    stop)
        /userdata/bin/usb-ethernet-ctl stop
        ;;
    *)
        /userdata/bin/usb-ethernet-ctl start &
        ;;
esac
EOF
chmod 755 /userdata/init.d/S99usb_ethernet.sh

# 5. Start immediately
echo "[4/4] Starting USB Ethernet..."
/userdata/bin/usb-ethernet-ctl start

echo ""
echo "=================================================="
echo "    Installation Successful!                      "
echo "=================================================="
echo "Check status anytime with: /userdata/bin/usb-ethernet-ctl status"
