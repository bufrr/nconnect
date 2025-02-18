package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eycorsican/go-tun2socks/core"
	"github.com/eycorsican/go-tun2socks/proxy/dnsfallback"
	"github.com/eycorsican/go-tun2socks/proxy/socks"
	"github.com/imdario/mergo"
	"github.com/jessevdk/go-flags"
	"github.com/nknorg/nconnect/admin"
	"github.com/nknorg/nconnect/config"
	"github.com/nknorg/nconnect/ss"
	"github.com/nknorg/nconnect/util"
	"github.com/nknorg/ncp-go"
	"github.com/nknorg/nkn-sdk-go"
	ts "github.com/nknorg/nkn-tuna-session"
	tunnel "github.com/nknorg/nkn-tunnel"
	"github.com/nknorg/nkn/v2/common"
	"github.com/nknorg/nkn/v2/util/address"
	"github.com/nknorg/nkngomobile"
	"github.com/nknorg/tuna"
	"github.com/nknorg/tuna/filter"
	"github.com/nknorg/tuna/geo"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	mtu = 1500
)

var opts struct {
	Client bool `short:"c" long:"client" description:"Client mode"`
	Server bool `short:"s" long:"server" description:"Server mode"`

	config.Config
	ConfigFile string `short:"f" long:"config-file" default:"config.json" description:"Config file path"`

	Address       bool `long:"address" description:"Print client address (client mode) or admin address (server mode)"`
	WalletAddress bool `long:"wallet-address" description:"Print wallet address (server only)"`
	Version       bool `long:"version" description:"Print version"`
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			log.Fatalf("Panic: %+v", r)
		}
	}()

	_, err := flags.Parse(&opts)
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		log.Fatal(err)
	}

	err = (&opts.Config).SetPlatformSpecificDefaultValues()
	if err != nil {
		log.Fatal(err)
	}

	if opts.Version {
		fmt.Println(config.Version)
		os.Exit(0)
	}

	if opts.Client == opts.Server {
		log.Fatal("Exactly one mode (client or server) should be selected.")
	}

	persistConf, err := config.LoadOrNewConfig(opts.ConfigFile)
	if err != nil {
		log.Fatal(err)
	}

	err = mergo.Merge(&opts.Config, persistConf)
	if err != nil {
		log.Fatal(err)
	}

	if len(opts.LogFileName) > 0 {
		log.SetOutput(&lumberjack.Logger{
			Filename:   opts.LogFileName,
			MaxSize:    opts.LogMaxSize,
			MaxBackups: opts.LogMaxBackups,
		})
	}

	seed, err := hex.DecodeString(opts.Seed)
	if err != nil {
		log.Fatal(err)
	}

	account, err := nkn.NewAccount(seed)
	if err != nil {
		log.Fatal(err)
	}

	shouldSave := false
	if len(opts.Seed) == 0 {
		persistConf.Seed = hex.EncodeToString(account.Seed())
		opts.Seed = persistConf.Seed
		shouldSave = true
	}

	if len(opts.Identifier) == 0 {
		persistConf.Identifier = config.RandomIdentifier()
		opts.Identifier = persistConf.Identifier
		shouldSave = true
	}

	if shouldSave {
		err = persistConf.Save()
		if err != nil {
			log.Fatal(err)
		}
	}

	if opts.Address {
		addr := address.MakeAddressString(account.PubKey(), opts.Identifier)
		if opts.Server && len(opts.AdminIdentifier) > 0 {
			addr = opts.AdminIdentifier + "." + addr
		}
		fmt.Println(addr)
		os.Exit(0)
	}

	if opts.WalletAddress {
		if opts.Server {
			fmt.Println(account.WalletAddress())
		} else {
			fmt.Println("Wallet address will not be shown in client mode")
		}
		os.Exit(0)
	}

	var seedRPCServerAddr *nkngomobile.StringArray
	if len(opts.SeedRPCServerAddr) > 0 {
		seedRPCServerAddr = nkn.NewStringArray(opts.SeedRPCServerAddr...)
	}

	locations := make([]geo.Location, 0, len(opts.TunaCountry))
	for i := range opts.TunaCountry {
		countries := strings.Split(opts.TunaCountry[i], ",")
		l := make([]geo.Location, len(countries))
		for i := range countries {
			l[i].CountryCode = strings.TrimSpace(countries[i])
		}
		locations = append(locations, l...)
	}

	allowIps := make([]geo.Location, len(opts.TunaAllowIp))
	for i := range opts.TunaAllowIp {
		ips := strings.Split(opts.TunaAllowIp[i], ",")
		l := make([]geo.Location, len(ips))
		for i := range ips {
			l[i].IP = strings.TrimSpace(ips[i])
		}
		allowIps = append(allowIps, l...)
	}
	var allowedIP = append(locations, allowIps...)

	disallowedIP := make([]geo.Location, len(opts.TunaDisallowIp))
	for i := range opts.TunaDisallowIp {
		ips := strings.Split(opts.TunaDisallowIp[i], ",")
		l := make([]geo.Location, len(ips))
		for i := range ips {
			l[i].IP = strings.TrimSpace(ips[i])
		}
		disallowedIP = append(disallowedIP, l...)
	}

	allowedNknAddrs := make([]filter.NknClient, len(opts.TunaAllowNknAddr))
	for i := range opts.TunaAllowNknAddr {
		addrs := strings.Split(opts.TunaAllowNknAddr[i], ",")
		l := make([]filter.NknClient, len(addrs))
		for i := range addrs {
			l[i].Address = strings.TrimSpace(addrs[i])
		}
		allowedNknAddrs = append(allowedNknAddrs, l...)
	}

	disallowedNknAddrs := make([]filter.NknClient, len(opts.TunaDisallowNknAddr))
	for i := range opts.TunaDisallowNknAddr {
		addrs := strings.Split(opts.TunaDisallowNknAddr[i], ",")
		l := make([]filter.NknClient, len(addrs))
		for i := range addrs {
			l[i].Address = strings.TrimSpace(addrs[i])
		}
		disallowedNknAddrs = append(disallowedNknAddrs, l...)
	}

	clientConfig := &nkn.ClientConfig{
		SeedRPCServerAddr: seedRPCServerAddr,
	}
	walletConfig := &nkn.WalletConfig{
		SeedRPCServerAddr: seedRPCServerAddr,
	}
	dialConfig := &nkn.DialConfig{
		DialTimeout: opts.DialTimeout,
	}

	if isValidUrl(opts.TunaMaxPrice) {
		price, err := getRemotePrice(opts.TunaMaxPrice)
		if err != nil {
			log.Printf("Get remote price error: %v", err)
			price = config.FallbackTunaMaxPrice
		}
		log.Printf("Set dynamic price to %s", price)
		opts.TunaMaxPrice = price
	}

	tsConfig := &ts.Config{
		TunaMaxPrice:           opts.TunaMaxPrice,
		TunaMinNanoPayFee:      opts.TunaMinFee,
		TunaNanoPayFeeRatio:    opts.TunaFeeRatio,
		TunaIPFilter:           &geo.IPFilter{Allow: allowedIP, Disallow: disallowedIP},
		TunaNknFilter:          &filter.NknFilter{Allow: allowedNknAddrs, Disallow: disallowedNknAddrs},
		TunaServiceName:        opts.TunaServiceName,
		TunaDownloadGeoDB:      !opts.TunaDisableDownloadGeoDB,
		TunaGeoDBPath:          opts.TunaGeoDBPath,
		TunaMeasureBandwidth:   !opts.TunaDisableMeasureBandwidth,
		TunaMeasureStoragePath: opts.TunaMeasureStoragePath,
	}

	if opts.SessionWindowSize > 0 {
		clientConfig.SessionConfig = &ncp.Config{SessionWindowSize: opts.SessionWindowSize}
		tsConfig.SessionConfig = &ncp.Config{SessionWindowSize: opts.SessionWindowSize}
	}

	tunnelConfig := &tunnel.Config{
		AcceptAddrs:       nkn.NewStringArray(persistConf.AcceptAddrs...),
		ClientConfig:      clientConfig,
		WalletConfig:      walletConfig,
		DialConfig:        dialConfig,
		TunaSessionConfig: tsConfig,
		Verbose:           opts.Verbose,
	}

	port, err := util.GetFreePort()
	if err != nil {
		log.Fatal(err)
	}

	ssAddr := "127.0.0.1:" + strconv.Itoa(port)

	ssConfig := &ss.Config{
		TCP:      true,
		UDP:      false,
		UDPSocks: true,
		Cipher:   opts.Cipher,
		Password: opts.Password,
		Verbose:  opts.Verbose,
	}

	var tun *tunnel.Tunnel

	if opts.Client {
		err = (&opts.Config).VerifyClient()
		if err != nil {
			log.Fatal(err)
		}

		// Lazy create admin client to avoid unnecessary client creation.
		var adminClientCache *admin.Client
		getAdminClient := func() (*admin.Client, error) {
			if adminClientCache != nil {
				return adminClientCache, nil
			}
			c, err := admin.NewClient(account, clientConfig)
			if err != nil {
				return nil, err
			}
			// Wait for more sub-clients to connect
			time.Sleep(time.Second)
			adminClientCache = c
			return adminClientCache, nil
		}

		// Lazy get remote info to avoid unnecessary rpc call.
		var remoteInfoCache *admin.GetInfoJSON
		getRemoteInfo := func() (*admin.GetInfoJSON, error) {
			if remoteInfoCache != nil {
				return remoteInfoCache, nil
			}
			c, err := getAdminClient()
			if err != nil {
				return nil, err
			}
			remoteInfoCache, err = c.GetInfo(opts.RemoteAdminAddr)
			if err != nil {
				return nil, fmt.Errorf("Get remote server info error: %v. Please make sure server is online and accepting connections from this client address", err)
			}
			return remoteInfoCache, nil
		}

		remoteTunnelAddr := opts.RemoteTunnelAddr
		if len(remoteTunnelAddr) == 0 {
			remoteInfo, err := getRemoteInfo()
			if err != nil {
				log.Fatal(err)
			}
			remoteTunnelAddr = remoteInfo.Addr
		}

		var vpnCIDR []*net.IPNet
		if opts.VPN {
			vpnRoutes := opts.VPNRoute
			if len(vpnRoutes) == 0 {
				remoteInfo, err := getRemoteInfo()
				if err != nil {
					log.Fatal(err)
				}
				if len(remoteInfo.LocalIP.Ipv4) > 0 {
					vpnRoutes = make([]string, 0, len(remoteInfo.LocalIP.Ipv4))
					for _, ip := range remoteInfo.LocalIP.Ipv4 {
						if ip == opts.TunAddr || ip == opts.TunGateway {
							log.Printf("Skipping server's local IP %s in routes", ip)
							continue
						}
						vpnRoutes = append(vpnRoutes, fmt.Sprintf("%s/32", ip))
					}
				}
			}
			if len(vpnRoutes) > 0 {
				vpnCIDR = make([]*net.IPNet, len(vpnRoutes))
				for i, cidr := range vpnRoutes {
					_, cidr, err := net.ParseCIDR(cidr)
					if err != nil {
						log.Fatalf("Parse CIDR %s error: %v", cidr, err)
					}
					vpnCIDR[i] = cidr
				}
			}
		}

		proxyAddr, err := net.ResolveTCPAddr("tcp", opts.LocalSocksAddr)
		if err != nil {
			log.Fatalf("Invalid proxy server address: %v", err)
		}
		proxyHost := proxyAddr.IP.String()
		proxyPort := uint16(proxyAddr.Port)

		ssConfig.Client = ssAddr
		ssConfig.Socks = opts.LocalSocksAddr

		tun, err = tunnel.NewTunnel(account, opts.Identifier, ssAddr, remoteTunnelAddr, opts.Tuna, tunnelConfig)
		if err != nil {
			log.Fatal(err)
		}

		log.Println("Client NKN address:", tun.Addr().String())
		log.Println("Client socks proxy listen address:", opts.LocalSocksAddr)

		if opts.Tun || opts.VPN {
			tunDevice, err := OpenTunDevice(opts.TunName, opts.TunAddr, opts.TunGateway, opts.TunMask, opts.TunDNS, true)
			if err != nil {
				log.Fatalf("Failed to open TUN device: %v", err)
			}

			core.RegisterOutputFn(tunDevice.Write)

			core.RegisterTCPConnHandler(socks.NewTCPHandler(proxyHost, proxyPort))
			core.RegisterUDPConnHandler(dnsfallback.NewUDPHandler())

			lwipWriter := core.NewLWIPStack()

			go func() {
				_, err := io.CopyBuffer(lwipWriter, tunDevice, make([]byte, mtu))
				if err != nil {
					log.Fatalf("Failed to write data to network stack: %v", err)
				}
			}()

			log.Println("Started tun2socks")

			if opts.VPN {
				for _, dest := range vpnCIDR {
					log.Printf("Adding route %s", dest)
					out, err := addRouteCmd(dest, opts.TunGateway, opts.TunName)
					if len(out) > 0 {
						os.Stdout.Write(out)
					}
					if err != nil {
						os.Stdout.Write([]byte(util.ParseExecError(err)))
						os.Exit(1)
					}
					defer func(dest *net.IPNet) {
						log.Printf("Deleting route %s", dest)
						out, err := deleteRouteCmd(dest, opts.TunGateway, opts.TunName)
						if len(out) > 0 {
							os.Stdout.Write(out)
						}
						if err != nil {
							os.Stdout.Write([]byte(util.ParseExecError(err)))
						}
					}(dest)
				}
			}
		}
	}

	if opts.Server {
		err = (&opts.Config).VerifyServer()
		if err != nil {
			log.Fatal(err)
		}

		ssConfig.Server = ssAddr

		if opts.Tuna {
			minBalance, err := common.StringToFixed64(opts.TunaMinBalance)
			if err != nil {
				log.Fatal(err)
			}

			if minBalance > 0 {
				w, err := nkn.NewWallet(account, walletConfig)
				if err != nil {
					log.Fatal(err)
				}

				balance, err := w.Balance()
				if err != nil {
					log.Println("Fetch balance error:", err)
				} else if balance.ToFixed64() < minBalance {
					log.Printf("Wallet balance %s is less than minimal balance to enable tuna %s, tuna will not be enabled", balance.String(), opts.TunaMinBalance)
					opts.Tuna = false
				}
			}
		}

		tun, err = tunnel.NewTunnel(account, opts.Identifier, "", ssAddr, opts.Tuna, tunnelConfig)
		if err != nil {
			log.Fatal(err)
		}

		log.Println("Tunnel listen address:", tun.FromAddr())

		if len(opts.AdminIdentifier) > 0 {
			go func() {
				identifier := opts.AdminIdentifier
				if len(opts.Identifier) > 0 {
					identifier += "." + opts.Identifier
				}
				err := admin.StartNKNServer(account, identifier, clientConfig, tun, persistConf, &opts.Config)
				if err != nil {
					log.Fatal(err)
				}
				os.Exit(0)
			}()
			log.Println("Admin listening address:", opts.AdminIdentifier+"."+tun.FromAddr())
		}

		if len(opts.AdminHTTPAddr) > 0 {
			go func() {
				err := admin.StartWebServer(opts.AdminHTTPAddr, tun, persistConf, &opts.Config)
				if err != nil {
					log.Fatal(err)
				}
				os.Exit(0)
			}()
			log.Println("Admin web dashboard listening address:", opts.AdminHTTPAddr)
		}
	}

	go func() {
		err := ss.Start(ssConfig)
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	go func() {
		err := tun.Start()
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}

// isValidUrl tests a string to determine if it is a well-structured url or not.
func isValidUrl(toTest string) bool {
	_, err := url.ParseRequestURI(toTest)
	if err != nil {
		return false
	}

	u, err := url.Parse(toTest)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}

	return true
}

func getRemotePrice(url string) (string, error) {
	client := http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	price := strings.TrimSpace(string(b))
	_, _, err = tuna.ParsePrice(price)
	if err != nil {
		return "", err
	}
	return price, nil
}
