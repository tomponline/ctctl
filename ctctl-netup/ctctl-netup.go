// +build linux,cgo

package main

import (
	"flag"
	"fmt"
	"gopkg.in/lxc/go-lxc.v2"
	"io/ioutil"
	"log"
	"log/syslog"
	"os/exec"
	"strings"
)

var (
	lxcpath string
)

func init() {
	flag.Parse()
	log.SetFlags(0)
	syslogWriter, err := syslog.New(syslog.LOG_INFO, "lxcnetup")
	if err == nil {
		log.SetOutput(syslogWriter)
	}
}

func main() {
	ctDevRef := flag.Arg(0)
	ctName := flag.Arg(1)
	context := flag.Arg(3)
	devType := flag.Arg(4)
	hostDevName := flag.Arg(5)

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

	//Check ctDevRef in network up scripts
	for i := 0; i < len(c.ConfigItem("lxc.network")); i++ {
		upScript := c.ConfigItem(fmt.Sprintf("lxc.network.%d.script.up", i))
		upEndArg := " " + ctDevRef
		if strings.HasSuffix(upScript[0], upEndArg) {
			v4Ips = c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv4", i))
			v6Ips = c.ConfigItem(fmt.Sprintf("lxc.network.%d.ipv6", i))
			break //Found what we needed
		}
	}

	if len(v4Ips) <= 0 && len(v6Ips) <= 0 {
		log.Print("No IPs defined for CT Dev Ref: ", ctDevRef)
		return
	}

	ipAddrs := make([]ipAddr, 0)
	enrichIps(v4Ips, &ipAddrs)
	enrichIps(v6Ips, &ipAddrs)

	switch context {
	case "up":
		runUp(c, ctName, hostDevName, ipAddrs)
	case "down":
		runDown(c, ctName, hostDevName, ipAddrs)
	default:
		log.Fatal("Unknown context: ", context)
	}
}

type ipAddr struct {
	ip       string
	routeDev string
}

func enrichIps(ips []string, outIps *[]ipAddr) {
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
				*outIps = append(*outIps, ipAddr{
					ip:       ip,
					routeDev: parts[index+1],
				})
				break
			}
		}
	}
	return
}

func runUp(c *lxc.Container, ctName string, hostDevName string, ips []ipAddr) {
	log.Printf("LXC Net UP: %s %s %s", ctName, hostDevName, ips)

	//Enable proxy arp on host interface
	proxyArpFile := "/proc/sys/net/ipv4/conf/" + hostDevName + "/proxy_arp"
	if err := ioutil.WriteFile(proxyArpFile, []byte("1"), 0644); err != nil {
		log.Fatal("Error activating proxy arp on ", hostDevName, ": ", err)
	}
	log.Print("Activated proxy arp on ", hostDevName)

	//Enable proxy ndp on host interface
	proxyNdpFile := "/proc/sys/net/ipv6/conf/" + hostDevName + "/proxy_ndp"
	if err := ioutil.WriteFile(proxyNdpFile, []byte("1"), 0644); err != nil {
		log.Fatal("Error activating proxy ndp on ", hostDevName, ": ", err)
	}
	log.Print("Activated proxy ndp on ", hostDevName)

	//Add proxy entries to host interface for gateway IPs.
	proxyGateways := []string{"192.0.2.1", "fd60:eafa:54a9::1"}
	for _, gwIp := range proxyGateways {
		//Setup proxy arp for default IP route on host interface
		cmd := exec.Command("ip", "neigh", "replace", "proxy", gwIp, "dev", hostDevName)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding proxy IP address '", gwIp, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added proxy for IP ", gwIp, " on ", hostDevName)

	}

	//Add static route and proxy entry for each IP
	for _, ip := range ips {
		cmd := exec.Command("ip", "route", "add", ip.ip, "dev", hostDevName)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding static route for IP address '", ip.ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added static route for IP ", ip.ip, " to ", hostDevName)

		cmd = exec.Command("ip", "neigh", "replace", "proxy", ip.ip, "dev", ip.routeDev)
		if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
			log.Fatal("Error adding proxy for IP address '", ip.ip, "': ", err, " ", string(stdoutStderr))
		}
		log.Print("Added proxy for IP ", ip.ip, " on ", ip.routeDev)
	}
}

func runDown(c *lxc.Container, ctName string, hostDevName string, ips []ipAddr) {
}
