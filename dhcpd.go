package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"golang.org/x/sys/unix"
)

const (
	bootRequest = 1
	bootReply   = 2

	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5

	optSubnetMask       = 1
	optRouter           = 3
	optDNS              = 6
	optRequestedIP      = 50
	optLeaseTime        = 51
	optMessageType      = 53
	optServerID         = 54
	optParamRequestList = 55
	optEnd              = 255
)

var magicCookie = []byte{99, 130, 83, 99}

type Server struct {
	ifaceName string
	iface     *net.Interface
	serverIP  net.IP
	clientIP  net.IP
	mask      net.IP
	dnsIPs    []net.IP
	rawFd     int
}

func parseDNSList(dnsStr string) []net.IP {
	var ips []net.IP
	for _, s := range strings.Split(dnsStr, ",") {
		ip := net.ParseIP(strings.TrimSpace(s))
		if ip != nil && ip.To4() != nil {
			ips = append(ips, ip.To4())
		}
	}
	return ips
}

func getSystemDNS() []net.IP {
	var ips []net.IP
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				ip := net.ParseIP(fields[1])
				if ip != nil && ip.To4() != nil {
					ips = append(ips, ip.To4())
				}
			}
		}
	}
	if len(ips) == 0 {
		ips = append(ips, net.ParseIP("1.1.1.1").To4(), net.ParseIP("8.8.8.8").To4())
	}
	return ips
}

func checksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i < len(data)-1; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for (sum >> 16) > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func main() {
	ifaceName := flag.String("iface", "usb2", "Interface to bind (usb0, usb1, usb2)")
	serverIPStr := flag.String("ip", "192.168.42.1", "Server IP on interface")
	clientIPStr := flag.String("client-ip", "192.168.42.2", "Static client IP to assign")
	maskStr := flag.String("mask", "255.255.255.0", "Subnet mask")
	dnsStr := flag.String("dns", "", "Comma-separated DNS servers")
	flag.Parse()

	srvIP := net.ParseIP(*serverIPStr).To4()
	cliIP := net.ParseIP(*clientIPStr).To4()
	mask := net.ParseIP(*maskStr).To4()

	var dnsIPs []net.IP
	if *dnsStr != "" {
		dnsIPs = parseDNSList(*dnsStr)
	} else {
		dnsIPs = getSystemDNS()
	}

	iface, err := net.InterfaceByName(*ifaceName)
	if err != nil {
		log.Printf("Warning: interface %s not found yet: %v", *ifaceName, err)
	}

	// Create RAW packet socket for Ethernet frames
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_IP)))
	if err != nil {
		log.Fatalf("Failed to open raw AF_PACKET socket: %v", err)
	}

	if iface != nil {
		ll := unix.SockaddrLinklayer{
			Protocol: htons(unix.ETH_P_IP),
			Ifindex:  iface.Index,
		}
		if err := unix.Bind(fd, &ll); err != nil {
			log.Printf("Warning: failed to bind raw socket to %s: %v", *ifaceName, err)
		}
	}

	log.Printf("Starting JetKVM DHCP server on %s (server: %s, client: %s, mask: %s)", *ifaceName, srvIP, cliIP, mask)

	srv := &Server{
		ifaceName: *ifaceName,
		iface:     iface,
		serverIP:  srvIP,
		clientIP:  cliIP,
		mask:      mask,
		dnsIPs:    dnsIPs,
		rawFd:     fd,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down DHCP server...")
		unix.Close(fd)
		os.Exit(0)
	}()

	buf := make([]byte, 2048)
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return
		}
		fromLL, ok := from.(*unix.SockaddrLinklayer)
		if !ok {
			continue
		}
		if srv.iface != nil && fromLL.Ifindex != srv.iface.Index {
			continue
		}
		// buf[0..13] is Ethernet header
		if n < 14+20+8+240 { // Eth + IP + UDP + DHCP min
			continue
		}
		ipHdr := buf[14:]
		if ipHdr[9] != 17 { // UDP
			continue
		}
		udpHdr := ipHdr[20:]
		srcPort := binary.BigEndian.Uint16(udpHdr[0:2])
		dstPort := binary.BigEndian.Uint16(udpHdr[2:4])
		if dstPort != 67 || srcPort != 68 {
			continue
		}
		dhcpData := udpHdr[8:]
		packet := make([]byte, len(dhcpData))
		copy(packet, dhcpData)
		go srv.handlePacket(packet, buf[6:12], fromLL.Ifindex)
	}
}

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}

func (s *Server) handlePacket(data []byte, srcMac []byte, ifIndex int) {
	if len(data) < 240 {
		return
	}
	if data[0] != bootRequest {
		return
	}
	if !bytes.Equal(data[236:240], magicCookie) {
		return
	}

	options := make(map[byte][]byte)
	opts := data[240:]
	i := 0
	for i < len(opts) {
		code := opts[i]
		if code == 0 { // pad
			i++
			continue
		}
		if code == optEnd {
			break
		}
		if i+1 >= len(opts) {
			break
		}
		l := int(opts[i+1])
		if i+2+l > len(opts) {
			break
		}
		options[code] = opts[i+2 : i+2+l]
		i += 2 + l
	}

	msgTypeSlice, ok := options[optMessageType]
	if !ok || len(msgTypeSlice) == 0 {
		return
	}
	msgType := msgTypeSlice[0]

	xid := binary.BigEndian.Uint32(data[4:8])
	mac := data[28 : 28+int(data[2])]
	macStr := net.HardwareAddr(mac).String()

	switch msgType {
	case dhcpDiscover:
		log.Printf("DHCPDISCOVER from %s (XID: 0x%x), sending DHCPOFFER %s", macStr, xid, s.clientIP)
		s.sendReply(dhcpOffer, data, xid, ifIndex, srcMac)
	case dhcpRequest:
		reqIPStr := ""
		if rip, ok := options[optRequestedIP]; ok && len(rip) == 4 {
			reqIPStr = fmt.Sprintf(" (requested %s)", net.IP(rip).String())
		}
		log.Printf("DHCPREQUEST from %s (XID: 0x%x)%s, sending DHCPACK %s", macStr, xid, reqIPStr, s.clientIP)
		s.sendReply(dhcpAck, data, xid, ifIndex, srcMac)
	default:
		log.Printf("DHCP message type %d from %s", msgType, macStr)
	}
}

func (s *Server) sendReply(replyType byte, req []byte, xid uint32, ifIndex int, dstMac []byte) {
	dhcp := make([]byte, 300)
	dhcp[0] = bootReply
	dhcp[1] = 1 // Ethernet
	dhcp[2] = 6 // MAC len
	dhcp[3] = 0 // Hops
	binary.BigEndian.PutUint32(dhcp[4:8], xid)
	binary.BigEndian.PutUint16(dhcp[8:10], 0)        // Secs
	binary.BigEndian.PutUint16(dhcp[10:12], 0x8000) // Broadcast flag

	copy(dhcp[16:20], s.clientIP.To4())
	copy(dhcp[20:24], s.serverIP.To4())
	copy(dhcp[28:44], req[28:44])
	copy(dhcp[236:240], magicCookie)

	optBuf := new(bytes.Buffer)
	optBuf.Write([]byte{optMessageType, 1, replyType})
	optBuf.Write([]byte{optServerID, 4})
	optBuf.Write(s.serverIP.To4())
	optBuf.Write([]byte{optLeaseTime, 4, 0x00, 0x01, 0x51, 0x80}) // 24h
	optBuf.Write([]byte{optSubnetMask, 4})
	optBuf.Write(s.mask.To4())
	optBuf.Write([]byte{optRouter, 4})
	optBuf.Write(s.serverIP.To4())

	if len(s.dnsIPs) > 0 {
		dnsBytes := []byte{}
		for _, dip := range s.dnsIPs {
			dnsBytes = append(dnsBytes, dip.To4()...)
		}
		optBuf.WriteByte(optDNS)
		optBuf.WriteByte(byte(len(dnsBytes)))
		optBuf.Write(dnsBytes)
	}

	optBuf.WriteByte(optEnd)

	dhcpPayload := append(dhcp[:240], optBuf.Bytes()...)

	// Build UDP header
	udpLen := 8 + len(dhcpPayload)
	udpHdr := make([]byte, 8)
	binary.BigEndian.PutUint16(udpHdr[0:2], 67)
	binary.BigEndian.PutUint16(udpHdr[2:4], 68)
	binary.BigEndian.PutUint16(udpHdr[4:6], uint16(udpLen))
	binary.BigEndian.PutUint16(udpHdr[6:8], 0) // checksum optional in IPv4

	// Build IP header
	ipLen := 20 + udpLen
	ipHdr := make([]byte, 20)
	ipHdr[0] = 0x45 // IPv4, 5 words
	ipHdr[1] = 0x00 // DSCP
	binary.BigEndian.PutUint16(ipHdr[2:4], uint16(ipLen))
	binary.BigEndian.PutUint16(ipHdr[4:6], 0x1234) // ID
	ipHdr[6] = 0x00
	ipHdr[7] = 0x00
	ipHdr[8] = 64 // TTL
	ipHdr[9] = 17 // UDP
	copy(ipHdr[12:16], s.serverIP.To4())
	copy(ipHdr[16:20], net.IPv4bcast.To4())
	ipChecksum := checksum(ipHdr)
	binary.BigEndian.PutUint16(ipHdr[10:12], ipChecksum)

	// Build Ethernet header
	ethHdr := make([]byte, 14)
	copy(ethHdr[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // Broadcast MAC
	if s.iface != nil {
		copy(ethHdr[6:12], s.iface.HardwareAddr)
	}
	binary.BigEndian.PutUint16(ethHdr[12:14], unix.ETH_P_IP)

	frame := append(ethHdr, ipHdr...)
	frame = append(frame, udpHdr...)
	frame = append(frame, dhcpPayload...)

	ll := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  ifIndex,
		Halen:    6,
	}
	copy(ll.Addr[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	err := unix.Sendto(s.rawFd, frame, 0, ll)
	if err != nil {
		log.Printf("Error sending raw Ethernet DHCP reply: %v", err)
	}
}
