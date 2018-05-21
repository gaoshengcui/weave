package net

import (
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/weaveworks/weave/common/odp"
)

/* This code implements three possible configurations to connect
   containers to the Weave Net overlay:

1. Bridge
                 +-------+
(container-veth)-+ weave +-(vethwe-bridge)--(vethwe-pcap)
                 +-------+

"weave" is a Linux bridge. "vethwe-pcap" (end of veth pair) is used
to capture and inject packets, by router/pcap.go.

2. BridgedFastdp

                 +-------+                                    /----------\
(container-veth)-+ weave +-(vethwe-bridge)--(vethwe-datapath)-+ datapath +
                 +-------+                                    \----------/

"weave" is a Linux bridge and "datapath" is an Open vSwitch datapath;
they are connected via a veth pair. Packet capture and injection use
the "datapath" device, via "router/fastdp.go:fastDatapathBridge"

3. Fastdp

                 /-------\
(container-veth)-+ weave +
                 \-------/

"weave" is an Open vSwitch datapath, and capture/injection are as in
BridgedFastdp. Not used by default due to missing conntrack support in
datapath of old kernel versions (https://github.com/weaveworks/weave/issues/1577).
*/

const (
	WeaveBridgeName = "weave"
	DatapathName    = "datapath"
	DatapathIfName  = "vethwe-datapath"
	BridgeIfName    = "vethwe-bridge"
	PcapIfName      = "vethwe-pcap"
)

type Bridge interface {
	init(config *BridgeConfig) error // create and initialise bridge device(s)
	attach(veth *netlink.Veth) error // attach veth to bridge
	IsFastdp() bool                  // does this bridge use fastdp?
	String() string                  // human-readable type string
}

// Used to indicate a fallback to the Bridge type
var errBridgeNotSupported = errors.New("bridge not supported")

type bridgeImpl struct{ bridge netlink.Link }
type fastdpImpl struct{ datapathName string }
type bridgedFastdpImpl struct {
	bridgeImpl
	fastdpImpl
}

// Returns a string that is consistent with the weave script
func (bridgeImpl) String() string        { return "bridge" }
func (fastdpImpl) String() string        { return "fastdp" }
func (bridgedFastdpImpl) String() string { return "bridged_fastdp" }

// Used to decide whether to manage ODP tunnels
func (bridgeImpl) IsFastdp() bool        { return false }
func (fastdpImpl) IsFastdp() bool        { return true }
func (bridgedFastdpImpl) IsFastdp() bool { return true }

func ExistingBridgeType(weaveBridgeName, datapathName string) (Bridge, error) {
	bridge, _ := netlink.LinkByName(weaveBridgeName)
	datapath, _ := netlink.LinkByName(datapathName)

	switch {
	case bridge == nil && datapath == nil:
		return nil, nil
	case isBridge(bridge) && datapath == nil:
		return bridgeImpl{bridge: bridge}, nil
	case isDatapath(bridge) && datapath == nil:
		return fastdpImpl{datapathName: datapathName}, nil
	case isDatapath(datapath) && isBridge(bridge):
		return bridgedFastdpImpl{bridgeImpl{bridge: bridge}, fastdpImpl{datapathName: datapathName}}, nil
	default:
		return nil, errors.New("Inconsistent bridge state detected. Please do 'weave reset' and try again")
	}
}

func EnforceAddrAssignType(bridgeName string) (setAddr bool, err error) {
	sysctlFilename := filepath.Join("/sys/class/net/", bridgeName, "/addr_assign_type")
	addrAssignType, err := ioutil.ReadFile(sysctlFilename)
	if err != nil {
		return false, errors.Wrapf(err, "reading %q", sysctlFilename)
	}

	// From include/uapi/linux/netdevice.h
	// #define NET_ADDR_PERM       0   /* address is permanent (default) */
	// #define NET_ADDR_RANDOM     1   /* address is generated randomly */
	// #define NET_ADDR_STOLEN     2   /* address is stolen from other device */
	// #define NET_ADDR_SET        3   /* address is set using dev_set_mac_address() */
	// Note the file typically has a newline at the end, so we just look at the first char
	if addrAssignType[0] != '3' {
		link, err := netlink.LinkByName(bridgeName)
		if err != nil {
			return false, errors.Wrapf(err, "EnforceAddrAssignType finding bridge %s", bridgeName)
		}

		mac, err := RandomMAC()
		if err != nil {
			return false, errors.Wrap(err, "creating random MAC")
		}

		if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
			return false, errors.Wrapf(err, "setting bridge %s address to %v", bridgeName, mac)
		}
		return true, nil
	}

	return false, nil
}

func isBridge(link netlink.Link) bool {
	_, isBridge := link.(*netlink.Bridge)
	return isBridge
}

func isDatapath(link netlink.Link) bool {
	switch link.(type) {
	case *netlink.GenericLink:
		return link.Type() == "openvswitch"
	case *netlink.Device:
		// Assume it's our openvswitch device, and the kernel has not been updated to report the kind.
		return true
	default:
		return false
	}
}

func DetectHairpin(portIfName string, log *logrus.Logger) error {
	link, err := netlink.LinkByName(portIfName)
	if err != nil {
		return fmt.Errorf("Unable to find link %q: %s", portIfName, err)
	}

	ch := make(chan netlink.LinkUpdate)
	// See EnsureInterface for why done channel is not passed
	if err := netlink.LinkSubscribe(ch, nil); err != nil {
		return fmt.Errorf("Unable to subscribe to netlink updates: %s", err)
	}

	pi, err := netlink.LinkGetProtinfo(link)
	if err != nil {
		return fmt.Errorf("Unable to get link protinfo %q: %s", portIfName, err)
	}
	if pi.Hairpin {
		return fmt.Errorf("Hairpin mode enabled on %q", portIfName)
	}

	go func() {
		for up := range ch {
			if up.Attrs().Name == portIfName && up.Attrs().Protinfo != nil &&
				up.Attrs().Protinfo.Hairpin {
				log.Errorf("Hairpin mode enabled on %q", portIfName)
			}
		}
	}()

	return nil
}

var ErrBridgeNoIP = fmt.Errorf("Bridge has no IP address")

func FindBridgeIP(bridgeName string, subnet *net.IPNet) (net.IP, error) {
	netdev, err := GetBridgeNetDev(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("Failed to get netdev for %q bridge: %s", bridgeName, err)
	}
	if len(netdev.CIDRs) == 0 {
		return nil, ErrBridgeNoIP
	}
	if subnet != nil {
		for _, cidr := range netdev.CIDRs {
			if subnet.Contains(cidr.IP) {
				return cidr.IP, nil
			}
		}
	}
	// No subnet, or none in the required subnet; just return the first one
	return netdev.CIDRs[0].IP, nil
}

type BridgeConfig struct {
	DockerBridgeName string
	WeaveBridgeName  string
	DatapathName     string
	NoFastdp         bool
	NoBridgedFastdp  bool
	AWSVPC           bool
	NPC              bool
	MTU              int
	Mac              string
	Port             int
}

func (config *BridgeConfig) configuredBridgeType() Bridge {
	switch {
	case config.NoFastdp:
		return bridgeImpl{}
	case config.NoBridgedFastdp:
		return fastdpImpl{datapathName: config.WeaveBridgeName}
	default:
		return bridgedFastdpImpl{fastdpImpl: fastdpImpl{datapathName: config.DatapathName}}
	}
}

func EnsureBridge(procPath string, config *BridgeConfig, log *logrus.Logger) (Bridge, error) {
	existingBridgeType, err := ExistingBridgeType(config.WeaveBridgeName, config.DatapathName)
	if err != nil {
		return nil, err
	}

	bridgeType := config.configuredBridgeType()

	if existingBridgeType != nil && bridgeType.String() != existingBridgeType.String() {
		return nil,
			fmt.Errorf("Existing bridge type %q is different than requested %q. Please do 'weave reset' and try again",
				existingBridgeType, bridgeType)
	}

	for {
		if err := bridgeType.init(config); err != nil {
			if errors.Cause(err) == errBridgeNotSupported {
				log.Warnf("Skipping bridge creation of %q due to: %s", bridgeType, err)
				bridgeType = bridgeImpl{}
				continue
			}
			return nil, err
		}
		break
	}

	if err := configureIPTables(config); err != nil {
		return bridgeType, errors.Wrap(err, "configuring iptables")
	}

	if config.AWSVPC {
		// Set proxy_arp on the bridge, so that it could accept packets destined
		// to containers within the same subnet but running on remote hosts.
		// Without it, exact routes on each container are required.
		if err := sysctl(procPath, "net/ipv4/conf/"+config.WeaveBridgeName+"/proxy_arp", "1"); err != nil {
			return bridgeType, errors.Wrap(err, "setting proxy_arp")
		}
		// Avoid delaying the first ARP request. Also, setting it to 0 avoids
		// placing the request into a bounded queue as it can be seen:
		// https://git.kernel.org/cgit/linux/kernel/git/stable/linux-stable.git/tree/net/ipv4/arp.c?id=refs/tags/v4.6.1#n819
		if err := sysctl(procPath, "net/ipv4/neigh/"+config.WeaveBridgeName+"/proxy_delay", "0"); err != nil {
			return bridgeType, errors.Wrap(err, "setting proxy_arp")
		}
	}

	if err := linkSetUpByName(config.WeaveBridgeName); err != nil {
		return bridgeType, err
	}

	if err := ConfigureARPCache(procPath, config.WeaveBridgeName); err != nil {
		return bridgeType, errors.Wrapf(err, "configuring ARP cache on bridge %q", config.WeaveBridgeName)
	}

	return bridgeType, nil
}

func (b bridgeImpl) initPrep(config *BridgeConfig) error {
	mac, err := net.ParseMAC(config.Mac)
	if err != nil {
		return errors.Wrapf(err, "parsing bridge MAC %q", config.Mac)
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = config.WeaveBridgeName
	linkAttrs.HardwareAddr = mac
	if config.MTU == 0 {
		config.MTU = 65535
	}
	b.bridge = &netlink.Bridge{LinkAttrs: linkAttrs}
	if err := LinkAddIfNotExist(b.bridge); err != nil {
		return errors.Wrapf(err, "creating bridge %q", config.WeaveBridgeName)
	}
	if err := netlink.LinkSetHardwareAddr(b.bridge, mac); err != nil {
		return errors.Wrapf(err, "setting bridge %q mac %v", config.WeaveBridgeName, mac)
	}
	// Attempting to set the bridge MTU to a high value directly
	// fails. Bridges take the lowest MTU of their interfaces. So
	// instead we create a temporary interface with the desired MTU,
	// attach that to the bridge, and then remove it again.
	dummy := &netlink.Dummy{LinkAttrs: netlink.NewLinkAttrs()}
	dummy.LinkAttrs.Name = "vethwedu"
	if err = netlink.LinkAdd(dummy); err != nil {
		return errors.Wrap(err, "creating dummy interface")
	}
	if err := netlink.LinkSetMTU(dummy, config.MTU); err != nil {
		return errors.Wrapf(err, "setting dummy interface mtu to %d", config.MTU)
	}
	if err := netlink.LinkSetMasterByIndex(dummy, b.bridge.Attrs().Index); err != nil {
		return errors.Wrap(err, "setting dummy interface master")
	}
	if err := netlink.LinkDel(dummy); err != nil {
		return errors.Wrap(err, "deleting dummy interface")
	}

	return nil
}

func (b bridgeImpl) init(config *BridgeConfig) error {
	if err := b.initPrep(config); err != nil {
		return err
	}
	if _, err := CreateAndAttachVeth(BridgeIfName, PcapIfName, config.WeaveBridgeName, config.MTU, true, false, func(veth netlink.Link) error {
		return netlink.LinkSetUp(veth)
	}); err != nil {
		return errors.Wrap(err, "creating pcap veth pair")
	}
	if err := EthtoolTXOff(config.WeaveBridgeName); err != nil {
		return errors.Wrap(err, "setting tx off")
	}

	return nil
}

func (f fastdpImpl) init(config *BridgeConfig) error {
	odpSupported, err := odp.CreateDatapath(f.datapathName)
	if !odpSupported {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		return errors.Wrap(errBridgeNotSupported, msg)
	}
	if err != nil {
		return errors.Wrapf(err, "creating datapath %q", f.datapathName)
	}
	datapath, err := netlink.LinkByName(f.datapathName)
	if err != nil {
		return errors.Wrapf(err, "finding datapath %q", f.datapathName)
	}
	if config.MTU == 0 {
		/* GCE has the lowest underlay network MTU we're likely to encounter on
		   a local network, at 1460 bytes.  To get the overlay MTU from that we
		   subtract 20 bytes for the outer IPv4 header, 8 bytes for the outer
		   UDP header, 8 bytes for the vxlan header, and 14 bytes for the inner
		   ethernet header.  In addition, we subtract 34 bytes for the ESP overhead
		   which is needed for the vxlan encryption. */
		config.MTU = 1376
	}
	if err := netlink.LinkSetMTU(datapath, config.MTU); err != nil {
		return errors.Wrapf(err, "setting datapath %q mtu %d", f.datapathName, config.MTU)
	}
	return nil
}

func (bf bridgedFastdpImpl) init(config *BridgeConfig) error {
	if err := bf.fastdpImpl.init(config); err != nil {
		return err
	}
	if err := bf.bridgeImpl.initPrep(config); err != nil {
		return err
	}
	if _, err := CreateAndAttachVeth(BridgeIfName, DatapathIfName, config.WeaveBridgeName, config.MTU, true, false, func(veth netlink.Link) error {
		if err := netlink.LinkSetUp(veth); err != nil {
			return errors.Wrapf(err, "setting link up on %q", veth.Attrs().Name)
		}
		if err := odp.AddDatapathInterfaceIfNotExist(bf.datapathName, veth.Attrs().Name); err != nil {
			return errors.Wrapf(err, "adding interface %q to datapath %q", veth.Attrs().Name, bf.datapathName)
		}
		return nil
	}); err != nil {
		return errors.Wrap(err, "creating bridged fastdp veth pair")
	}

	return linkSetUpByName(bf.datapathName)
}

func (b bridgeImpl) attach(veth *netlink.Veth) error {
	return netlink.LinkSetMasterByIndex(veth, b.bridge.Attrs().Index)
}

func (bf bridgedFastdpImpl) attach(veth *netlink.Veth) error {
	return bf.bridgeImpl.attach(veth)
}

func (f fastdpImpl) attach(veth *netlink.Veth) error {
	return odp.AddDatapathInterfaceIfNotExist(f.datapathName, veth.Attrs().Name)
}

func configureIPTables(config *BridgeConfig) error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.Wrap(err, "creating iptables object")
	}

	// The order among weave filter/FORWARD rules is important!
	fwdRules := make([][]string, 0)

	if config.DockerBridgeName != "" {
		if config.WeaveBridgeName != config.DockerBridgeName {
			fwdRules = append(fwdRules, []string{"-i", config.DockerBridgeName, "-o", config.WeaveBridgeName, "-j", "DROP"})
		}

		dockerBridgeIP, err := FindBridgeIP(config.DockerBridgeName, nil)
		if err != nil {
			return err
		}

		// forbid traffic to the Weave port from other containers
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.DockerBridgeName, "-p", "tcp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port), "-j", "DROP"); err != nil {
			return err
		}
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port), "-j", "DROP"); err != nil {
			return err
		}
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port+1), "-j", "DROP"); err != nil {
			return err
		}

		// let DNS traffic to weaveDNS, since otherwise it might get blocked by the likes of UFW
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dport", "53", "-j", "ACCEPT"); err != nil {
			return err
		}
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.DockerBridgeName, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"); err != nil {
			return err
		}
	}

	if config.NPC {
		// Steer traffic via the NPC.
		// Ignore error because chains might already exist
		// TODO(brb): use constants for WEAVE-NPC* chains
		_ = ipt.NewChain("filter", "WEAVE-NPC")
		_ = ipt.NewChain("filter", "WEAVE-NPC-EGRESS")

		// TODO(brb): this rule might slow down packet processing; consider removing at it applies only
		// to node-local traffic.
		if err = ipt.AppendUnique("filter", "INPUT", "-i", config.WeaveBridgeName, "-j", "WEAVE-NPC-EGRESS"); err != nil {
			return err
		}

		// If WEAVE-NPC chain doesn't exist then creating a rule in the chain will fail
		fwdRules = append(fwdRules,
			[][]string{
				{"-i", config.WeaveBridgeName,
					"-m", "comment", "--comment", "NOTE: this must go before '-j KUBE-FORWARD'",
					"-j", "WEAVE-NPC-EGRESS"},
				{"-o", config.WeaveBridgeName,
					"-m", "comment", "--comment", "NOTE: this must go before '-j KUBE-FORWARD'",
					"-j", "WEAVE-NPC"},
				{"-o", config.WeaveBridgeName, "-m", "state", "--state", "NEW", "-j", "NFLOG", "--nflog-group", "86"},
				{"-o", config.WeaveBridgeName, "-j", "DROP"},
			}...)
	} else {
		// Work around the situation where there are no rules allowing traffic
		// across our bridge. E.g. ufw
		fwdRules = append(fwdRules, []string{"-i", config.WeaveBridgeName, "-o", config.WeaveBridgeName, "-j", "ACCEPT"})
	}

	if !config.NPC {
		// Create a chain for allowing ingress traffic when the bridge is exposed
		_ = ipt.NewChain("filter", "WEAVE-EXPOSE")
		fwdRules = append(fwdRules, []string{"-o", config.WeaveBridgeName, "-j", "WEAVE-EXPOSE"})
	}

	// Forward from weave to the rest of the world
	fwdRules = append(fwdRules, []string{"-i", config.WeaveBridgeName, "!", "-o", config.WeaveBridgeName, "-j", "ACCEPT"})
	// and allow replies back
	fwdRules = append(fwdRules, []string{"-o", config.WeaveBridgeName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"})

	if err := ensureRulesAtTop("filter", "FORWARD", fwdRules, ipt); err != nil {
		return err
	}

	// create a chain for masquerading
	//
	// NB: we do not clear the chain to preserve existing rules
	// inserted by "weave expose".
	_ = ipt.NewChain("nat", "WEAVE")

	return ipt.AppendUnique("nat", "POSTROUTING", "-j", "WEAVE")
}

func linkSetUpByName(linkName string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return errors.Wrapf(err, "setting link up on %q", linkName)
	}
	return netlink.LinkSetUp(link)
}

// ensureRulesAtTop ensures the presence of given iptables rules.
//
// If any rule from the list is missing, the function deletes all given
// rules and re-inserts them at the top of the chain to ensure the order of the rules.
func ensureRulesAtTop(table, chain string, rulespecs [][]string, ipt *iptables.IPTables) error {
	allFound := true

	for _, rs := range rulespecs {
		found, err := ipt.Exists(table, chain, rs...)
		if err != nil {
			return errors.Wrapf(err, "ipt.Exists(%s, %s, %s)", table, chain, rs)
		}
		if !found {
			allFound = false
			break
		}
	}

	// All rules exist, do nothing.
	if allFound {
		return nil
	}

	for pos, rs := range rulespecs {
		// If any is missing, then delete all, as we need to preserve the order of
		// given rules. Ignore errors, as rule might not exist.
		if !allFound {
			ipt.Delete(table, chain, rs...)
		}
		if err := ipt.Insert(table, chain, pos+1, rs...); err != nil {
			return errors.Wrapf(err, "ipt.Append(%s, %s, %s)", table, chain, rs)
		}
	}

	return nil
}
