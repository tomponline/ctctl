// +build linux,cgo

package main

import (
	"flag"
	"fmt"
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

type ipAddr struct {
	ip       string
	routeDev string
}

type proxyDev struct {
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

	proxyDevs := make(map[string]proxyDev)

	//Check for which network has correct up script
	for i := 0; i < len(c.ConfigItem("lxc.network")); i++ {
		upScript := c.ConfigItem(fmt.Sprintf("lxc.network.%d.script.up", i))
		//Check up script is this program.
		if strings.HasSuffix(upScript[0], os.Args[0]) {
			v4Ips = c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv4", i))
			v6Ips = c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv6", i))

			hostProxyDev := proxyDev{
				dev:   hostDevName,
				gwIps: make([]string, 0),
			}
			v4Gws := c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv4.gateway", i))
			if v4Gws[0] != "" {
				hostProxyDev.gwIps = append(hostProxyDev.gwIps, v4Gws[0])
			}
			v6Gws := c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv6.gateway", i))
			if v6Gws[0] != "" {
				hostProxyDev.gwIps = append(hostProxyDev.gwIps, v6Gws[0])
			}

			proxyDevs[hostDevName] = hostProxyDev
			break //Found what we needed
		}
	}

	if len(v4Ips) <= 0 && len(v6Ips) <= 0 {
		log.Fatal("No IPs defined for CT Dev Ref: ", ctName)
	}

	if len(proxyDevs[hostDevName].gwIps) <= 0 {
		log.Fatal("No Gateways defined for CT Dev Ref: ", ctName)
	}

	ipAddrs := make([]ipAddr, 0)
	enrichIps(v4Ips, &ipAddrs, proxyDevs)
	enrichIps(v6Ips, &ipAddrs, proxyDevs)

	switch context {
	case "up":
		runUp(c, ctName, hostDevName, ipAddrs, proxyDevs)
	case "down":
		runDown(c, ctName, hostDevName, ipAddrs, proxyDevs)
	default:
		log.Fatal("Unknown context: ", context)
	}
}

func enrichIps(ips []string, outIps *[]ipAddr, outDevs map[string]proxyDev) {
	for _, ip := range ips {
		//Convert to IP without netmask.
		index := strings.Index(ip, "/")
		if index < 0 {
			log.Print("Invalid IP/mask supplied: ", ip)
			continue
		}

		ip := ip[:index] //Strip subnet from IP

		//Figure out interface to add proxy arp for
		cmd := exec.Command("ip", "-o", "route", "get", ip)
		stdoutStderr, err := cmd.CombinedOutput()
		if err != nil {
			log.Print(err, string(stdoutStderr))
			continue
		}

		parts := strings.Fields(string(stdoutStderr))
		for index, val := range parts {
			if val == "dev" {
				devName := parts[index+1]
				*outIps = append(*outIps, ipAddr{
					ip:       ip,
					routeDev: devName,
				})
				outDevs[devName] = proxyDev{
					dev: devName,
				}
				break
			}
		}
	}
	return
}

func activateProxyNdp(dev string) error {
	//Enable proxy ndp on  interface (needed before adding specific proxy entries)
	proxyNdpFile := "/proc/sys/net/ipv6/conf/" + dev + "/proxy_ndp"
	return ioutil.WriteFile(proxyNdpFile, []byte("1"), 0644)
}

func activateNonLocalBind() error {
	//Enable non-local bind.
	proxyNdpFile := "/proc/sys/net/ipv4/ip_nonlocal_bind"
	return ioutil.WriteFile(proxyNdpFile, []byte("1"), 0644)
}

func runUp(c *lxc.Container, ctName string, hostDevName string, ips []ipAddr, proxyDevs map[string]proxyDev) {
	log.Printf("LXC Net UP: %s %s %s", ctName, hostDevName, ips)

	//Activate IPv6 proxy ndp on all interfaces to ensure IPv6 connectivity works.
	//There is some unexpected behaviour when proxy ndp is only enabled on selected interfaces
	//that does not occur with proxy arp for IPv4.
	if err := activateProxyNdp("all"); err != nil {
		log.Fatal("Error activating proxy ndp: ", err)
	}
	log.Print("Activated proxy ndp")

	for _, proxyDev := range proxyDevs {
		for _, gwIp := range proxyDev.gwIps {
			//Setup proxy arp for default IP route on host interface
			cmd := exec.Command("ip", "neigh", "replace", "proxy", gwIp, "dev", proxyDev.dev)
			if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
				log.Fatal("Error adding proxy IP '", gwIp, "': ", err, " ", string(stdoutStderr))
			}
			log.Print("Added proxy for IP ", gwIp, " on ", proxyDev.dev)

		}
	}

	//Enable non-local bind for ARP send.
	if err := activateNonLocalBind(); err != nil {
		log.Fatal("Error activating non-local bind: ", err)
	}
	log.Print("Activated non-local bind")

	//Add static route and proxy entry for each IP
	for _, ip := range ips {
		cmd := exec.Command("ip", "route", "add", ip.ip, "dev", hostDevName)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding static route for IP '", ip.ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added static route for IP ", ip.ip, " to ", hostDevName)

		cmd = exec.Command("ip", "neigh", "replace", "proxy", ip.ip, "dev", ip.routeDev)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding proxy for IP '", ip.ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added proxy for IP ", ip.ip, " on ", ip.routeDev)

		//Send NDP or ARP (IPv6 and IPv4 respectively) adverts
		if strings.Contains(ip.ip, ":") {
			cmd = exec.Command("ndsend", ip.ip, ip.routeDev)
		} else {
			cmd = exec.Command("arping", "-c1", "-A", ip.ip, "-I", ip.routeDev)
		}

		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error sending NDP/ARP for IP '", ip.ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Advertised NDP/ARP for IP '", ip.ip, "' on ", ip.routeDev)
	}
}

func runDown(c *lxc.Container, ctName string, hostDevName string, ips []ipAddr, proxyDevs map[string]proxyDev) {
}
