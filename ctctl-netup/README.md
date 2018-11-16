# Usage

This tool is designed to be used as an LXC network hook script to emulate OpenVZ style venet networking.
It uses a veth pair between the host and the container, and then uses proxy ARP and NDP on the host
to advertise the container's IP addresses to the network.

It uses an embedded gratuitous ARP and NDP advertisement system so no external dependencies are needed.

Example Usage:

```
# Network configuration
lxc.net.0.type = veth
lxc.net.0.flags = up
lxc.net.0.name = eth0
lxc.net.0.ipv4.address = 192.168.31.3/32
lxc.net.0.ipv4.gateway = 169.254.0.1
lxc.net.0.ipv6.address = 2a00:1098:0:xx:xxxx::3/128
lxc.net.0.ipv6.gateway = fe80::1
lxc.net.0.script.up = /usr/libexec/lxc/ctctl-netup
lxc.net.0.script.down = /usr/libexec/lxc/ctctl-netup
```

Note:

The IP addresses defined must be a /32 (IPv4) or /128 (IPv6) respectively.
The gateway IPs have been selected on purpose as "unused" addresses that do not exist in the external network.
They are just used as a way to get packets from the container the host system. 
