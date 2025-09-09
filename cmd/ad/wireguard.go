package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/tinyrange/wireguard"
)

func GenerateRandomString(length int) (string, error) {
	b := make([]byte, length)

	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

type WireguardInstance interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	ConfigUrl() string
}

type wireguardInstance struct {
	internalIp string
	configUrl  string
	wg         *wireguard.Wireguard
}

// DialContext implements WireguardInstance.
func (w *wireguardInstance) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	if w.internalIp != "" {
		host = w.internalIp
	}

	slog.Debug("dialing wireguard", "host", host, "port", port)

	conn, err := w.wg.DialContext(ctx, network, net.JoinHostPort(host, port))
	if err != nil {
		slog.Error("failed to DialContext", "address", address, "err", err)
		return nil, err
	}

	slog.Debug("dialed wireguard", "address", address, "err", err)

	return conn, nil
}

func (w *wireguardInstance) ConfigUrl() string {
	return w.configUrl
}

var (
	_ WireguardInstance = &wireguardInstance{}
)

type WireguardRouter interface {
	AddEndpoint(handler NetHandler, internalIp string) (WireguardInstance, error)
	AddDevice(handler NetHandler) (inst WireguardInstance, config string, err error)

	RegisterMux(mux *http.ServeMux)
}

// A wireguard router that generates wireguard configurations.
type wireguardRouter struct {
	mtx             sync.Mutex
	listenAddress   string
	externalAddress string
	mtu             int
	serverUrl       string
	configSalt      string
	configs         map[string]configEntry
	handlers        map[string]NetHandler
	wg              *wireguard.Wireguard
}

type configEntry struct {
	config   string
	hostname string
}

func (r *wireguardRouter) configKeyFromHostname(hostname string) string {
	hash := sha256.New()
	hash.Write([]byte(r.configSalt))
	hash.Write([]byte(hostname))
	return hex.EncodeToString(hash.Sum(nil))
}

func (r *wireguardRouter) AddEndpoint(handler NetHandler, internalIp string) (WireguardInstance, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	slog.Info("adding wireguard endpoint", "instance", handler, "ip", handler.IpAddress().String())

	peerConfig, err := r.wg.CreatePeer(r.listenAddress, handler.IpAddress().String())
	if err != nil {
		return nil, err
	}

	configKey := r.configKeyFromHostname(handler.Hostname())

	r.configs[configKey] = configEntry{
		config:   peerConfig,
		hostname: handler.Hostname(),
	}

	r.handlers[handler.IpAddress().String()] = handler
	return &wireguardInstance{wg: r.wg, internalIp: internalIp, configUrl: fmt.Sprintf("%s/wireguard/%s", r.serverUrl, configKey)}, nil
}

func (r *wireguardRouter) serveConfig(w http.ResponseWriter, req *http.Request) {
	configKey := req.PathValue("config")

	slog.Debug("serving wireguard config", "config", configKey)

	r.mtx.Lock()
	config, ok := r.configs[configKey]
	r.mtx.Unlock()
	if !ok {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}

	// Set the content type to plain text
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.conf\"", config.hostname))

	if _, err := w.Write([]byte(config.config)); err != nil {
		slog.Error("failed to write config", "err", err)
	}
}

func (r *wireguardRouter) RegisterMux(mux *http.ServeMux) {
	mux.HandleFunc("GET /wireguard/{config}", r.serveConfig)
}

func (r *wireguardRouter) translateToDeviceConfig(ip string, peerConfig string) (string, error) {
	var (
		privateKey string
		publicKey  string
		endpoint   string
		listenPort string
	)

	for _, line := range strings.Split(peerConfig, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			if line == "" {
				continue
			}
			return "", fmt.Errorf("invalid line: %s", line)
		}

		switch k {
		case "private_key":
			privateKey = v
		case "public_key":
			publicKey = v
		case "endpoint":
			endpoint = v
		case "listen_port":
			listenPort = v
		}
	}

	// convert private and public key from hex to base64
	privateKeyBytes, err := hex.DecodeString(privateKey)
	if err != nil {
		return "", err
	}

	privateKey = base64.StdEncoding.EncodeToString(privateKeyBytes)

	publicKeyBytes, err := hex.DecodeString(publicKey)
	if err != nil {
		return "", err
	}

	publicKey = base64.StdEncoding.EncodeToString(publicKeyBytes)

	var config string
	if listenPort == "" {
		config = fmt.Sprintf(`[Interface]
Address = %s
PrivateKey = %s
MTU = %d

[Peer]
PublicKey = %s
AllowedIPs = 10.40.0.0/16
Endpoint = %s
`,
			ip, privateKey, r.mtu, publicKey, endpoint,
		)
	} else {
		config = fmt.Sprintf(`[Interface]
Address = %s
PrivateKey = %s
MTU = %d

[Peer]
PublicKey = %s
AllowedIPs = 10.40.0.0/16
Endpoint = %s:%s
`,
			ip, privateKey, r.mtu, publicKey, r.externalAddress, listenPort,
		)
	}

	return config, nil
}

func (r *wireguardRouter) AddDevice(handler NetHandler) (inst WireguardInstance, config string, err error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	peerConfig, err := r.wg.CreatePeer(r.externalAddress, handler.IpAddress().String())
	if err != nil {
		return nil, "", err
	}

	deviceConfig, err := r.translateToDeviceConfig(handler.IpAddress().String(), peerConfig)
	if err != nil {
		return nil, "", err
	}

	configKey := r.configKeyFromHostname(handler.Hostname())
	r.configs[configKey] = configEntry{
		config:   deviceConfig,
		hostname: handler.Hostname(),
	}

	r.handlers[handler.IpAddress().String()] = handler
	inst = &wireguardInstance{configUrl: fmt.Sprintf("%s/wireguard/%s", r.serverUrl, configKey), wg: r.wg}
	config = peerConfig

	return
}

func filterConfigToKeys(config string, keys []string) (string, error) {
	var lines []string

	for _, line := range strings.Split(config, "\n") {
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			if line == "" {
				continue
			}
			return "", fmt.Errorf("invalid line: %s", line)
		}

		for _, key := range keys {
			if k == key {
				lines = append(lines, line)
				break
			}
		}
	}

	return strings.Join(lines, "\n"), nil
}

func NewWireguardRouter(listenAddress string, externalAddress string, mtu int, serverUrl string, persist *PersistDatabase, devices []struct {
	Config  string
	Handler NetHandler
	Device  *Device
}) (WireguardRouter, error) {
	salt, err := GenerateRandomString(8)
	if err != nil {
		return nil, err
	}

	router := &wireguardRouter{
		listenAddress:   listenAddress,
		externalAddress: externalAddress,
		mtu:             mtu,
		serverUrl:       serverUrl,
		configs:         make(map[string]configEntry),
		configSalt:      salt,
		handlers:        make(map[string]NetHandler),
	}

	state, err := persist.GetPersistentState()
	if err != nil {
		return nil, err
	}

	var wg *wireguard.Wireguard
	if state.WireguardServerConfig == nil {
		wg, err = wireguard.NewServer(HOST_IP, router.mtu, router)
		if err != nil {
			return nil, err
		}

		config, err := wg.GetConfig()
		if err != nil {
			return nil, err
		}

		if len(devices) != 0 {
			slog.Warn("Existing devices ignored (server key pair regenerated)")
		}

		persist.UpdateWireguardServerConfig(&config)
	} else {
		config := *state.WireguardServerConfig

		configs := make([]string, 0)
		configs = append(configs, config)

		for _, device := range devices {
			slog.Info("Adding device", "conf", device.Config)
			filteredConfig, err := filterConfigToKeys(device.Config, []string{"public_key"})
			if err != nil {
				return nil, err
			}
			configs = append(configs, filteredConfig)
			configs = append(configs, fmt.Sprintf("\nallowed_ip=%s/32\n", device.Device.InstanceAddress().String()))
			configs = append(configs, "preshared_key=0000000000000000000000000000000000000000000000000000000000000000\n")
			configs = append(configs, "protocol_version=1\n")

			deviceConfig, err := router.translateToDeviceConfig(device.Handler.IpAddress().String(), device.Config)
			if err != nil {
				return nil, err
			}

			configKey := router.configKeyFromHostname(device.Handler.Hostname())
			router.configs[configKey] = configEntry{
				config:   deviceConfig,
				hostname: device.Handler.Hostname(),
			}

			inst := &wireguardInstance{configUrl: fmt.Sprintf("%s/wireguard/%s", router.serverUrl, configKey), wg: wg}
			device.Device.wg = inst
		}

		config = strings.Join(configs, "")
		wg, err = wireguard.NewFromConfig(HOST_IP, router.mtu, config, router)

		if err != nil {
			return nil, err
		}
	}

	router.wg = wg
	return router, nil
}

// String implements NetHandler.
func (r *wireguardRouter) String() string {
	return "router"
}

// IpAddress implements NetHandler.
func (r *wireguardRouter) IpAddress() net.IP {
	return net.ParseIP(r.listenAddress)
}

// String implements NetHandler.
func (r *wireguardRouter) Hostname() string {
	return "router"
}

// HandleConn implements NetHandler
func (r *wireguardRouter) HandleConn(network string, ip net.IP, port uint16, conn net.Conn) {
	source := conn.RemoteAddr().String()
	if idx := strings.Index(source, ":"); idx != -1 {
		source = source[:idx]
	}

	handler := r.handlers[source]
	if handler == nil {
		slog.Error("no handler for source ip", "source_ip", source)
		return
	}

	slog.Info("found handler for source ip", "source_ip", source, "target", ip.String(), "port", port)
	handler.HandleConn(network, ip, port, conn)
}
