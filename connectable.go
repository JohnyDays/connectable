package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/fsouza/go-dockerclient"
	"github.com/progrium/connectable/pkg/lookup"
)

var Version string

var (
	self *docker.Container
)

func getopt(name, def string) string {
	if env := os.Getenv(name); env != "" {
		return env
	}
	return def
}

func assert(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func runNetCmd(container, image string, cmd []string) error {
	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)
	if err != nil {
		return err
	}
	c, err := client.CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image: image,
			Cmd:   cmd,
		},
		HostConfig: &docker.HostConfig{
			Privileged:  true,
			NetworkMode: fmt.Sprintf("container:%s", container),
		},
	})
	if err != nil {
		return err
	}
	err = client.StartContainer(c.ID, nil)
	if err != nil {
		return err
	}
	status, err := client.WaitContainer(c.ID)
	if err != nil {
		return err
	}
	if status != 0 {
		return fmt.Errorf("non-zero exit: %v", status)
	}
	return client.RemoveContainer(docker.RemoveContainerOptions{
		ID:    c.ID,
		Force: true,
	})
}

func originalDestinationPort(conn net.Conn) (string, error) {
	f, err := conn.(*net.TCPConn).File()
	if err != nil {
		return "", err
	}
	defer f.Close()
	addr, err := syscall.GetsockoptIPv6Mreq(
		int(f.Fd()), syscall.IPPROTO_IP, 80) // 80 = SO_ORIGINAL_DST
	if err != nil {
		return "", err
	}
	port := uint16(addr.Multiaddr[2])<<8 + uint16(addr.Multiaddr[3])
	return strconv.Itoa(int(port)), nil
}

func inspectBackend(sourceIP, destPort string) (string, error) {
	label := fmt.Sprintf("connect[%s]", destPort)

	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)
	if err != nil {
		return "", err
	}

	// todo: cache, invalidate with container destroy events
	containers, err := client.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		return "", err
	}
	for _, listing := range containers {
		container, err := client.InspectContainer(listing.ID)
		if err != nil {
			return "", err
		}
		if container.NetworkSettings.IPAddress == sourceIP {
			backend, ok := container.Config.Labels[label]
			if !ok {
				return "", fmt.Errorf("connect label '%s' not found: %v", label, container.Config.Labels)
			}
			return backend, nil
		}
	}
	return "", fmt.Errorf("unable to find container with source IP")
}

func lookupBackend(conn net.Conn) string {
	sourceIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	destPort, err := originalDestinationPort(conn)
	if err != nil {
		log.Println("unable to determine destination port")
		return ""
	}

	backend, err := inspectBackend(sourceIP, destPort)
	if err != nil {
		log.Println(err)
		return ""
	}
	return backend
}

func proxyConn(conn net.Conn, addr string) {
	backend, err := net.Dial("tcp", addr)
	defer conn.Close()
	if err != nil {
		log.Println("proxy", err.Error())
		return
	}
	defer backend.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(backend, conn)
		backend.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(conn, backend)
	conn.(*net.TCPConn).CloseWrite()
	<-done
}

func setupContainer(id string) {
	re := regexp.MustCompile("connect\\[(\\d+)\\]")
	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)
	assert(err)
	container, err := client.InspectContainer(id)
	if err != nil {
		log.Println(err)
	}
	if container.HostConfig.NetworkMode == "bridge" {
		cmds := []string{
			"/sbin/sysctl -w net.ipv4.conf.all.route_localnet=1",
			"iptables -t nat -I POSTROUTING 1 -m addrtype --src-type LOCAL --dst-type UNICAST -j MASQUERADE",
		}
		for k, _ := range container.Config.Labels {
			results := re.FindStringSubmatch(k)
			if len(results) > 1 {
				cmds = append(cmds, fmt.Sprintf(
					"iptables -t nat -I OUTPUT 1 -m addrtype --src-type LOCAL --dst-type LOCAL -p tcp --dport %s -j DNAT --to-destination %s:%s",
					results[1], self.NetworkSettings.IPAddress, results[1]))
			}
		}
		shellCmd := strings.Join(cmds, " && ")
		assert(runNetCmd(container.ID, self.Image, []string{"/bin/sh", "-c", shellCmd}))
	}
}

func monitorContainers() {
	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)
	assert(err)
	events := make(chan *docker.APIEvents)
	assert(client.AddEventListener(events))
	list, _ := client.ListContainers(docker.ListContainersOptions{})
	for _, listing := range list {
		go setupContainer(listing.ID)
	}
	for msg := range events {
		switch msg.Status {
		case "create":
			go setupContainer(msg.ID)
		}
	}
}

func main() {
	flag.Parse()
	port := getopt("PORT", "10000")

	/*
		  var backends BackendProvider
			if flag.Arg(0) != "" {
				backends = NewBackendProvider(flag.Arg(0))
			} else {
				backends = NewOmniProvider()
			}
	*/

	listener, err := net.Listen("tcp", ":"+port)
	assert(err)

	log.Println("Connectable listening on", port, "...")

	endpoint := "unix:///var/run/docker.sock"
	client, _ := docker.NewClient(endpoint)

	list, _ := client.ListContainers(docker.ListContainersOptions{})
	for _, listing := range list {
		c, _ := client.InspectContainer(listing.ID)
		if c.Config.Hostname == os.Getenv("HOSTNAME") {
			self = c
			fmt.Println("Self:", c.ID, c.HostConfig.NetworkMode)
			if c.HostConfig.NetworkMode == "bridge" {
				shellCmd := fmt.Sprintf("iptables -t nat -A PREROUTING -p tcp -j REDIRECT --to-ports %s", port)
				assert(runNetCmd(c.ID, c.Image, []string{"/bin/sh", "-c", shellCmd}))
			}
		}
	}

	if self == nil {
		log.Fatal("unable to find self")
	}

	go monitorContainers()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}

		backend := lookupBackend(conn)
		if backend == "" {
			conn.Close()
			continue
		}

		backendAddrs, err := lookup.Resolve(backend)
		if err != nil {
			log.Println(err)
			conn.Close()
			continue
		}
		if len(backendAddrs) == 0 {
			conn.Close()
			continue
		}

		log.Println(conn.RemoteAddr(), "->", backendAddrs[0])
		go proxyConn(conn, backendAddrs[0])
	}
}
