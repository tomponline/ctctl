// +build linux,cgo

package main

import (
	"flag"
	"fmt"
	"github.com/tomponline/ctctl/internal/arp"
	"github.com/tomponline/ctctl/internal/ndp"
	"gopkg.in/lxc/go-lxc.v2"
	"io/ioutil"
	"log"
	"log/syslog"
	"os"
	"os/exec"
	"strings"
)

var (
	lxcpath string
)

func init() {
	flag.Parse()
	log.SetFlags(0)
	syslogWriter, err := syslog.New(syslog.LOG_INFO, "ctctl-netup")
	if err == nil {
		log.SetOutput(syslogWriter)
	}
}

type gwDev struct {
	dev   string
	gwIps []string
}

func main() {
	ctName := flag.Arg(0)
	context := flag.Arg(2)
	devType := flag.Arg(3)
	hostDevName := flag.Arg(4)

	c, err := lxc.NewContainer(ctName, lxc.DefaultConfigPath())
	if err != nil {
		log.Fatalf("Cannot load config for: %s, %s\n", ctName, err.Error())
	}

	if !c.Defined() {
		log.Fatalf("Container %s not defined", ctName)
	}

	if devType != "veth" {
		log.Fatal("Unsupported dev type: ", devType)
	}

	var v4Ips []string
	var v6Ips []string

	gwProxyDevs := make(map[string]gwDev)
	netPrefixes := []string{"lxc.net", "lxc.network"}

	for _, netPrefix := range netPrefixes {
		//Check for which network has correct up script
		for i := 0; i < len(c.ConfigItem(netPrefix)); i++ {
			upScript := c.ConfigItem(fmt.Sprintf("%s.%d.script.up", netPrefix, i))
			//Check up script is this program.
			if strings.HasSuffix(upScript[0], os.Args[0]) {
				if netPrefix == "lxc.network" {
					v4Ips = c.ConfigItem(fmt.Sprintf("%s.%d.ipv4", netPrefix, i))
					v6Ips = c.ConfigItem(fmt.Sprintf("%s.%d.ipv6", netPrefix, i))

				} else {
					v4Ips = c.ConfigItem(fmt.Sprintf("%s.%d.ipv4.address", netPrefix, i))
					v6Ips = c.ConfigItem(fmt.Sprintf("%s.%d.ipv6.address", netPrefix, i))
				}

				gwProxyDev := gwDev{
					dev:   hostDevName,
					gwIps: make([]string, 0),
				}

				v4Gws := c.ConfigItem(fmt.Sprintf("%s.%d.ipv4.gateway", netPrefix, i))
				if v4Gws[0] != "" {
					gwProxyDev.gwIps = append(gwProxyDev.gwIps, v4Gws[0])
				}
				v6Gws := c.ConfigItem(fmt.Sprintf("%s.%d.ipv6.gateway", netPrefix, i))
				if v6Gws[0] != "" {
					gwProxyDev.gwIps = append(gwProxyDev.gwIps, v6Gws[0])
				}

				gwProxyDevs[hostDevName] = gwProxyDev
				break //Found what we needed
			}
		}
	}

	if len(v4Ips) <= 0 && len(v6Ips) <= 0 {
		log.Fatal("No IPs defined for CT Dev Ref: ", ctName)
	}

	if len(gwProxyDevs[hostDevName].gwIps) <= 0 {
		log.Fatal("No Gateways defined for CT Dev Ref: ", ctName)
	}

	ipAddrs := make([]string, 0, 2)
	cleanIps(v4Ips, &ipAddrs)
	cleanIps(v6Ips, &ipAddrs)

	switch context {
	case "up":
		runUp(c, ctName, hostDevName, ipAddrs, gwProxyDevs)
	case "down":
		runDown(c, ctName, hostDevName, ipAddrs)
	default:
		log.Fatal("Unknown context: ", context)
	}
}

func cleanIps(ips []string, outIps *[]string) {
	for _, ip := range ips {
		//Convert to IP without netmask.
		index := strings.Index(ip, "/")
		if index < 0 {
			log.Print("Invalid IP/mask supplied: ", ip)
			continue
		}

		ip := ip[:index] //Strip subnet from IP
		*outIps = append(*outIps, ip)
	}

}

func getRouteDev(ip string) string {
	//Figure out interface to add proxy arp for
	cmd := exec.Command("ip", "-o", "route", "get", ip)
	stdoutStderr, err := cmd.CombinedOutput()
	if err != nil {
		log.Print(err, string(stdoutStderr))
		return ""
	}

	parts := strings.Fields(string(stdoutStderr))
	for index, val := range parts {
		if val == "dev" {
			devName := parts[index+1]
			return devName
		}
	}

	return ""
}

func activateProxyNdp(dev string) error {
	//Enable proxy ndp on  interface (needed before adding specific proxy entries)
	proxyNdpFile := "/proc/sys/net/ipv6/conf/" + dev + "/proxy_ndp"
	return ioutil.WriteFile(proxyNdpFile, []byte("1"), 0644)
}

func runUp(c *lxc.Container, ctName string, hostDevName string, ips []string, gwProxyDevs map[string]gwDev) {
	log.Printf("LXC Net UP: %s %s %s", ctName, hostDevName, ips)

	//Activate IPv6 proxy ndp on all interfaces to ensure IPv6 connectivity works.
	//There is some unexpected behaviour when proxy ndp is only enabled on selected interfaces
	//that does not occur with proxy arp for IPv4.
	if err := activateProxyNdp("all"); err != nil {
		log.Fatal("Error activating proxy ndp: ", err)
	}
	log.Print("Activated proxy ndp")

	for _, gwDev := range gwProxyDevs {
		for _, gwIp := range gwDev.gwIps {
			//Setup proxy arp for default IP route on host interface
			cmd := exec.Command("ip", "neigh", "replace", "proxy", gwIp, "dev", gwDev.dev)
			if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
				log.Fatal("Error adding proxy IP '", gwIp, "': ", err, " ", string(stdoutStderr))
			}
			log.Print("Added proxy for IP ", gwIp, " on ", gwDev.dev)

		}
	}

	//Add static route and proxy entry for each IP
	for _, ip := range ips {
		//Lookup current route dev so we can setup proxy arp/ndp.
		routeDev := getRouteDev(ip)

		if routeDev == "" {
			log.Fatal("Can't find route device for IP '", ip, "'")
		}

		cmd := exec.Command("ip", "route", "add", ip, "dev", hostDevName)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding static route for IP '", ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added static route for IP ", ip, " to ", hostDevName)

		cmd = exec.Command("ip", "neigh", "replace", "proxy", ip, "dev", routeDev)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding proxy for IP '", ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added proxy for IP ", ip, " on ", routeDev)

		//Send NDP or ARP (IPv6 and IPv4 respectively) adverts
		if strings.Contains(ip, ":") {
			if err := ndp.SendUnsolicited(routeDev, ip); err != nil {
				log.Fatal("Error sending NDP for IP '", ip, " on iface ", routeDev, ": ", err)
			}
		} else {
			if err := arp.SendUnsolicited(routeDev, ip); err != nil {
				log.Fatal("Error sending ARP for IP '", ip, " on iface ", routeDev, ": ", err)
			}
		}

		log.Print("Advertised NDP/ARP for IP '", ip, "' on ", routeDev)
	}
}

func runDown(c *lxc.Container, ctName string, hostDevName string, ips []string) {
	log.Printf("LXC Net Down: %s %s %s", ctName, hostDevName, ips)

	//Remove static route and proxy entry for each IP
	for _, ip := range ips {
		cmd := exec.Command("ip", "route", "del", ip, "dev", hostDevName)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error deleting static route for IP '", ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Deleted static route for IP ", ip, " to ", hostDevName)

		//Now static route is removed, find original route dev so we can remove proxy arp/ndp config.
		routeDev := getRouteDev(ip)

		if routeDev == "" {
			continue
		}

		cmd = exec.Command("ip", "neigh", "del", "proxy", ip, "dev", routeDev)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Print("Error remove proxy for IP '", ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Deleted proxy for IP ", ip, " on ", routeDev)
	}
}
