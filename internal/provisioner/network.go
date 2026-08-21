package provisioner

import (
	"fmt"
	"sync"
)

// vmCIDR is the parent range carved into per-microVM /30 subnets for the default
// network base (172.16). Each slot n gets 172.16.n.0/30: host (gateway) .1,
// guest .2. A single NAT masquerade rule for the whole range covers every
// microVM. Multiple firerunner instances on one host use distinct bases (see
// vmCIDRFor / slotNet's netBase) so their subnets never overlap.
const vmCIDR = "172.16.0.0/16"

// vmCIDRFor returns the parent /16 for a given network base (the second octet),
// e.g. netBase 16 -> "172.16.0.0/16", 17 -> "172.17.0.0/16".
func vmCIDRFor(netBase int) string {
	return fmt.Sprintf("172.%d.0.0/16", netBase)
}

// vmNet is the fully-resolved per-microVM network, derived purely from a slot.
type vmNet struct {
	slot     int
	tap      string
	hostIP   string // gateway, on the host side of the tap
	guestIP  string
	netmask  string
	guestMAC string
}

// slotNet returns the network parameters for a slot within the given network
// base (the second IP octet). It is pure so the IP/MAC derivation can be
// unit-tested. netBase lets multiple firerunner instances on one host carve
// non-overlapping /16s (e.g. 16 -> 172.16.x, 17 -> 172.17.x).
func slotNet(slot int, tapPrefix string, netBase int) vmNet {
	return vmNet{
		slot:     slot,
		tap:      fmt.Sprintf("%s%d", tapPrefix, slot),
		hostIP:   fmt.Sprintf("172.%d.%d.1", netBase, slot),
		guestIP:  fmt.Sprintf("172.%d.%d.2", netBase, slot),
		netmask:  "255.255.255.252",
		guestMAC: fmt.Sprintf("06:00:AC:%02x:%02x:02", netBase, slot),
	}
}

// composeBootArgs appends the kernel IP-autoconfiguration clause so the guest
// boots with a static address, gateway and netmask without needing DHCP. Format:
// ip=<client>:<server>:<gw>:<netmask>:<hostname>:<device>:<autoconf>.
func composeBootArgs(base string, n vmNet) string {
	return fmt.Sprintf("%s ip=%s::%s:%s::eth0:off", base, n.guestIP, n.hostIP, n.netmask)
}

// tapUpCommands brings up a per-microVM tap device with its host-side gateway IP.
func tapUpCommands(n vmNet) [][]string {
	return [][]string{
		{"ip", "tuntap", "add", "dev", n.tap, "mode", "tap"},
		{"ip", "addr", "add", n.hostIP + "/30", "dev", n.tap},
		{"ip", "link", "set", n.tap, "up"},
	}
}

// tapDownCommands removes a tap device.
func tapDownCommands(tap string) [][]string {
	return [][]string{{"ip", "link", "del", tap}}
}

// netnsName is the per-slot network namespace name, keyed by tap prefix so
// firerunner instances sharing a host never collide.
func netnsName(slot int, tapPrefix string) string {
	return fmt.Sprintf("%sns%d", tapPrefix, slot)
}

// netnsSpec is the per-slot netns network identity. The guest tap lives inside
// the namespace (its address is the guest's gateway); a veth pair on a distinct
// transit /30 links the namespace to the host, which routes the guest /32 back
// through it. Reusing the slot's own /24 (guest .0/30, transit .4/30) means no
// extra address base is needed.
type netnsSpec struct {
	ns       string // network namespace name
	hostVeth string // veth end in the host namespace
	nsVeth   string // veth end inside the netns
	hostVIP  string // host veth address (transit .5)
	nsVIP    string // netns veth address (transit .6, the netns default gw)
	nsPath   string // filesystem path to the named netns (jailer --netns)
}

// netnsRunDir is the iproute2 named-netns directory. A var (not const) so tests
// can redirect the stale-sweep away from the real /var/run/netns.
var netnsRunDir = "/var/run/netns"

// slotNetNS derives the netns network identity for a slot. It is pure so the
// setup/teardown command lists can be unit-tested.
func slotNetNS(n vmNet, netBase int, tapPrefix string) netnsSpec {
	return netnsSpec{
		ns:       netnsName(n.slot, tapPrefix),
		hostVeth: fmt.Sprintf("%sv%dh", tapPrefix, n.slot),
		nsVeth:   fmt.Sprintf("%sv%dg", tapPrefix, n.slot),
		hostVIP:  fmt.Sprintf("172.%d.%d.5", netBase, n.slot),
		nsVIP:    fmt.Sprintf("172.%d.%d.6", netBase, n.slot),
		nsPath:   netnsRunDir + "/" + netnsName(n.slot, tapPrefix),
	}
}

// netnsUpCommands builds the per-slot netns: a private namespace holding the
// guest tap (owned by uid:gid so the dropped-privilege VMM can attach it), a
// veth pair to the host on the transit /30, intra-netns forwarding, and a host
// route back to the guest so return traffic reaches it. uid/gid are the jail
// user's; the guest gateway is n.hostIP inside the namespace.
func netnsUpCommands(n vmNet, s netnsSpec, uid, gid int) [][]string {
	u, g := fmt.Sprintf("%d", uid), fmt.Sprintf("%d", gid)
	return [][]string{
		{"ip", "netns", "add", s.ns},
		{"ip", "netns", "exec", s.ns, "ip", "link", "set", "lo", "up"},
		// Guest tap inside the netns; its address is the guest's gateway.
		{"ip", "netns", "exec", s.ns, "ip", "tuntap", "add", "dev", n.tap, "mode", "tap", "user", u, "group", g},
		{"ip", "netns", "exec", s.ns, "ip", "addr", "add", n.hostIP + "/30", "dev", n.tap},
		{"ip", "netns", "exec", s.ns, "ip", "link", "set", n.tap, "up"},
		// veth pair linking the netns to the host on the transit /30.
		{"ip", "link", "add", s.hostVeth, "type", "veth", "peer", "name", s.nsVeth},
		{"ip", "link", "set", s.nsVeth, "netns", s.ns},
		{"ip", "addr", "add", s.hostVIP + "/30", "dev", s.hostVeth},
		{"ip", "link", "set", s.hostVeth, "up"},
		{"ip", "netns", "exec", s.ns, "ip", "addr", "add", s.nsVIP + "/30", "dev", s.nsVeth},
		{"ip", "netns", "exec", s.ns, "ip", "link", "set", s.nsVeth, "up"},
		// Route guest egress out of the netns and forward tap<->veth.
		{"ip", "netns", "exec", s.ns, "ip", "route", "add", "default", "via", s.hostVIP},
		{"ip", "netns", "exec", s.ns, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
		// Host route so un-NATed return traffic reaches the guest via the netns.
		{"ip", "route", "add", n.guestIP + "/32", "via", s.nsVIP},
	}
}

// ipam hands out and reclaims network slots, bounding concurrent microVMs and
// guaranteeing non-overlapping per-VM subnets.
type ipam struct {
	mu   sync.Mutex
	free []int
}

func newIPAM(size int) *ipam {
	free := make([]int, size)
	for i := range free {
		free[i] = i
	}
	return &ipam{free: free}
}

// acquire reserves a slot, returning false when the pool is exhausted.
func (p *ipam) acquire() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return 0, false
	}
	slot := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return slot, true
}

// release returns a slot to the pool.
func (p *ipam) release(slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, slot)
}
