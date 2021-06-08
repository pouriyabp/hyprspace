package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/DataDrake/cli-ng/v2/cmd"
	"github.com/hyprspace/hyprspace/config"
	"github.com/hyprspace/hyprspace/p2p"
	"github.com/hyprspace/hyprspace/tun"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/songgao/water"
	"golang.org/x/net/ipv4"
)

var (
	Global    config.Config
	iface     *water.Interface
	RevLookup map[string]bool
)

// Pull downloads files from the Arken cluster.
var Up = cmd.Sub{
	Name:  "up",
	Alias: "up",
	Short: "Create and Bring Up a Hyprspace Interface.",
	Args:  &UpArgs{},
	Flags: &UpFlags{},
	Run:   UpRun,
}

// UpArgs handles the specific arguments for the up command.
type UpArgs struct {
	InterfaceName string
}

type UpFlags struct {
	Forground bool `short:"f" long:"forground" desc:"Don't Create Background Daemon."`
}

func UpRun(r *cmd.Root, c *cmd.Sub) {
	// Parse Command Args
	args := c.Args.(*UpArgs)

	// Parse Command Flags
	flags := c.Flags.(*UpFlags)

	// Parse Global Config Flag for Custom Config Path
	configPath := r.Flags.(*GlobalFlags).Config
	if configPath == "" {
		configPath = "/etc/hyprspace/" + args.InterfaceName + ".yaml"
	}

	// Read in configuration from file.
	Global, err := config.Read(configPath)
	checkErr(err)

	if !flags.Forground {
		// Make results chan
		out := make(chan bool)
		go createDaemon(out)

		select {
		case <-out:
		case <-time.After(30 * time.Second):
		}
		checkErr(err)
		fmt.Println("[+] Sucessfully Created Hyprspace Daemon")
		return
	}

	// Setup reverse lookup hash map for authentication.
	RevLookup = make(map[string]bool, len(Global.Peers))
	for _, id := range Global.Peers {
		RevLookup[id.ID] = true
	}

	fmt.Println("[+] Creating TUN Device")
	// Create new TUN device
	iface, err = tun.New(Global.Interface.Name)
	checkErr(err)
	// Set TUN MTU
	tun.SetMTU(Global.Interface.Name, 1500)
	// Add Address to Interface
	tun.SetAddress(Global.Interface.Name, Global.Interface.Address)

	// Setup System Context
	ctx := context.Background()

	fmt.Println("[+] Creating LibP2P Node")
	// Create P2P Node
	host, dht, err := p2p.CreateNode(ctx, Global.Interface.PrivateKey, streamHandler)
	checkErr(err)

	// Setup Peer Table for Quick Packet --> Dest ID lookup
	peerTable := make(map[string]peer.ID)
	for ip, id := range Global.Peers {
		peerTable[ip], err = peer.Decode(id.ID)
		checkErr(err)
	}

	fmt.Println("[+] Setting Up Node Discovery via DHT")
	// Setup P2P Discovery
	go p2p.Discover(ctx, host, dht, Global.Interface.DiscoverKey, peerTable)
	go prettyDiscovery(ctx, host, peerTable)

	go func() {
		// Wait for a SIGINT or SIGTERM signal
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		fmt.Println("Received signal, shutting down...")

		// Shut the node down
		if err := host.Close(); err != nil {
			panic(err)
		}
		os.Exit(0)
	}()

	// Bring Up TUN Device
	tun.Up(Global.Interface.Name)

	fmt.Println("[+] Network Setup Complete...Waiting on Node Discovery")
	// Listen For New Packets on TUN Interface
	packet := make([]byte, 1500)
	for {
		plen, err := iface.Read(packet)
		checkErr(err)
		header, _ := ipv4.ParseHeader(packet[:plen])
		_, ok := Global.Peers[header.Dst.String()]
		if ok {
			stream, err := host.NewStream(context.Background(), peerTable[header.Dst.String()], p2p.Protocol)
			if err != nil {
				log.Println(err)
				continue
			}

			go func() {
				stream.Write(packet[:plen])
				stream.Close()
			}()
		}
	}
}

func createDaemon(out chan<- bool) {
	path, err := os.Executable()
	checkErr(err)
	// Create Pipe to monitor for daemon output.
	r, w, err := os.Pipe()
	checkErr(err)
	// Create Sub Process
	process, err := os.StartProcess(
		path,
		append(os.Args, "--forground"),
		&os.ProcAttr{
			Files: []*os.File{nil, w, nil},
		},
	)
	checkErr(err)
	scanner := bufio.NewScanner(r)
	count := 0
	for scanner.Scan() && count < (4+len(Global.Peers)) {
		fmt.Println(scanner.Text())
		count++
	}
	fmt.Println(scanner.Text())
	err = process.Release()
	checkErr(err)
	out <- true
}

func streamHandler(stream network.Stream) {
	// If the remote node ID isn't in the list of known nodes don't respond.
	if _, ok := RevLookup[stream.Conn().RemotePeer().Pretty()]; !ok {
		stream.Close()
	}
	io.Copy(iface.ReadWriteCloser, stream)
	stream.Close()
}

func prettyDiscovery(ctx context.Context, node host.Host, peerTable map[string]peer.ID) {
	tempTable := make(map[string]peer.ID, len(peerTable))
	for ip, id := range peerTable {
		tempTable[ip] = id
	}
	for len(peerTable) > 0 {
		for ip, id := range tempTable {
			stream, err := node.NewStream(ctx, id, p2p.Protocol)
			if err != nil && (strings.HasPrefix(err.Error(), "failed to dial") ||
				strings.HasPrefix(err.Error(), "no addresses")) {
				time.Sleep(5 * time.Second)
				continue
			}
			if err == nil {
				fmt.Printf("[+] Connection to %s Sucessful. Network Ready.\n", ip)
				stream.Close()
			}
			delete(tempTable, ip)
		}
	}
}