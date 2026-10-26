package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	version             = "0.1.0"
	xrayVersion         = "26.3.27"
	defaultHysteriaPort = 18443
	defaultSocksPort    = 1080
	defaultDomain       = "reverse-node.internal"
	portalTag           = "reverse-node-portal"
	socksTag            = "reverse-node-socks"
	hysteriaInboundTag  = "reverse-node-hysteria-in"
	bridgeTag           = "reverse-node-bridge"
	bridgeOutboundTag   = "reverse-node-hysteria"
	defaultBrutalUp     = "16mbps"
	defaultBrutalDown   = "24mbps"
)

var domainLabel = regexp.MustCompile("^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$")
var brutalRatePattern = regexp.MustCompile("^([1-9][0-9]{0,4})(\\.([0-9]{1,2}))?mbps$")
var sha256Pattern = regexp.MustCompile("^[0-9a-fA-F]{64}$")

type NodeSettings struct {
	Version           int    "json:\"version\""
	XrayPath          string "json:\"xrayPath\""
	PublicHost        string "json:\"publicHost\""
	Listen            string "json:\"listen\""
	Port              int    "json:\"port\""
	SocksListen       string "json:\"socksListen\""
	SocksPort         int    "json:\"socksPort\""
	Domain            string "json:\"domain\""
	Auth              string "json:\"auth\""
	BrutalUp          string "json:\"brutalUp\""
	BrutalDown        string "json:\"brutalDown\""
	BridgeEgress      string "json:\"bridgeEgress\""
	LogLevel          string "json:\"logLevel\""
	CertFile          string "json:\"certFile\""
	KeyFile           string "json:\"keyFile\""
	PinCertificate    bool   "json:\"pinCertificate\""
	CertificateSHA256 string "json:\"certificateSha256,omitempty\""
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "reverse_node:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stdout)
		return nil
	}
	switch args[0] {
	case "init":
		return initCommand(args[1:])
	case "run":
		return runCommand(args[1:])
	case "check":
		return checkCommand(args[1:])
	case "print-bridge":
		return printBridgeCommand(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("reverse_node %s (Xray-core %s)\n", version, xrayVersion)
		return nil
	case "help", "--help", "-h":
		printUsage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run reverse_node help", args[0])
	}
}

func printUsage(out *os.File) {
	fmt.Fprintln(out, "reverse_node - Windows reverse SOCKS5 Portal using native Xray Hysteria2/QUIC Brutal\n\n"+
		"Commands:\n"+
		"  init         Generate the domestic Portal config and matching overseas Bridge fragment.\n"+
		"  check        Validate the generated config with Xray-core 26.3.27 without starting it.\n"+
		"  run          Start Xray and restart it if it exits unexpectedly.\n"+
		"  print-bridge Print the generated overseas Bridge config fragment.\n"+
		"  version      Print reverse_node and Xray compatibility versions.\n\n"+
		"The domestic node is the Portal. The overseas Xray Bridge actively connects to the\n"+
		"domestic DDNS hostname over Hysteria2/QUIC. Local SOCKS5 stays on loopback only.\n\n"+
		"Required for init:\n"+
		"  reverse_node init --xray-path C:\\xray\\xray.exe --public-host node.example.net\n\n"+
		"Use \"reverse_node <command> --help\" for command-specific flags.")
}

func defaultStateDir() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = "."
	}
	return filepath.Join(base, "reverse_node")
}

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg NodeSettings
	var stateDir, certPath, keyPath, suppliedAuth string
	var pinCert, force bool
	fs.StringVar(&cfg.XrayPath, "xray-path", "", "Path to xray.exe (required)")
	fs.StringVar(&cfg.XrayPath, "xray", "", "Alias for --xray-path")
	fs.StringVar(&cfg.PublicHost, "public-host", "", "Domestic DDNS hostname or public IP advertised to the overseas Bridge")
	fs.StringVar(&cfg.Listen, "listen", "::", "Address for the public Hysteria2 UDP listener")
	fs.IntVar(&cfg.Port, "port", defaultHysteriaPort, "Public Hysteria2 UDP port")
	fs.StringVar(&cfg.SocksListen, "socks-listen", "127.0.0.1", "Local SOCKS5 bind address (loopback only)")
	fs.IntVar(&cfg.SocksPort, "socks-port", defaultSocksPort, "Local SOCKS5 TCP/UDP port")
	fs.StringVar(&cfg.Domain, "domain", defaultDomain, "Private Xray reverse bridge/portal routing domain")
	fs.StringVar(&suppliedAuth, "auth", "", "Optional shared Hysteria2 auth; generated securely when omitted")
	fs.StringVar(&cfg.BrutalUp, "brutal-up", defaultBrutalUp, "Domestic upload target, e.g. 16mbps")
	fs.StringVar(&cfg.BrutalDown, "brutal-down", defaultBrutalDown, "Domestic download target, e.g. 24mbps")
	fs.StringVar(&cfg.BridgeEgress, "bridge-egress", "freedom", "Fallback outbound tag on the overseas Xray Bridge")
	fs.StringVar(&cfg.LogLevel, "log-level", "warning", "Xray log level: error, warning, info or debug")
	fs.StringVar(&certPath, "cert", "", "Optional existing TLS certificate PEM path (default: generate a self-signed certificate)")
	fs.StringVar(&keyPath, "key", "", "Optional existing TLS private key PEM path")
	fs.BoolVar(&pinCert, "pin-cert", false, "Pin an existing TLS certificate in the Bridge fragment; generated self-signed certs are always pinned")
	fs.StringVar(&stateDir, "state-dir", defaultStateDir(), "Directory for private state, certificates and generated configs")
	fs.BoolVar(&force, "force", false, "Replace existing state and rotate generated auth/certificate material")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(cfg.XrayPath) == "" {
		return errors.New("--xray-path is required")
	}
	if strings.TrimSpace(cfg.PublicHost) == "" {
		return errors.New("--public-host is required; use the domestic DDNS hostname")
	}
	var err error
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(stateDir, "node.json")); err == nil && !force {
		return fmt.Errorf("state already exists at %s; use --force to rotate credentials and overwrite it", stateDir)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	cfg.Version = 1
	cfg.XrayPath, err = filepath.Abs(cfg.XrayPath)
	if err != nil {
		return err
	}
	cfg.PublicHost = strings.TrimSpace(strings.TrimSuffix(cfg.PublicHost, "."))
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.SocksListen = strings.TrimSpace(cfg.SocksListen)
	cfg.Domain = strings.ToLower(strings.TrimSpace(cfg.Domain))
	cfg.BrutalUp, err = normalizeBrutalRate(cfg.BrutalUp)
	if err != nil {
		return fmt.Errorf("--brutal-up: %w", err)
	}
	cfg.BrutalDown, err = normalizeBrutalRate(cfg.BrutalDown)
	if err != nil {
		return fmt.Errorf("--brutal-down: %w", err)
	}
	cfg.BridgeEgress = strings.TrimSpace(cfg.BridgeEgress)
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	if suppliedAuth == "" {
		cfg.Auth, err = randomToken()
		if err != nil {
			return err
		}
	} else {
		cfg.Auth = suppliedAuth
	}
	if err := verifyXray(cfg.XrayPath); err != nil {
		return err
	}
	if (certPath == "") != (keyPath == "") {
		return errors.New("--cert and --key must be provided together")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	generatedCert := certPath == ""
	if generatedCert {
		cfg.CertFile = filepath.Join(stateDir, "tls-cert.pem")
		cfg.KeyFile = filepath.Join(stateDir, "tls-key.pem")
		if err := generateSelfSignedCertificate(cfg.PublicHost, cfg.CertFile, cfg.KeyFile); err != nil {
			return err
		}
		cfg.PinCertificate = true
	} else {
		cfg.CertFile, err = filepath.Abs(certPath)
		if err != nil {
			return err
		}
		cfg.KeyFile, err = filepath.Abs(keyPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(cfg.CertFile); err != nil {
			return fmt.Errorf("TLS certificate: %w", err)
		}
		if _, err := os.Stat(cfg.KeyFile); err != nil {
			return fmt.Errorf("TLS private key: %w", err)
		}
		cfg.PinCertificate = pinCert || !certificateTrusted(cfg.CertFile, cfg.PublicHost)
	}
	if cfg.PinCertificate {
		cfg.CertificateSHA256, err = certificateSHA256(cfg.CertFile)
		if err != nil {
			return err
		}
	}
	if err := validateSettings(cfg); err != nil {
		return err
	}
	portalPath := filepath.Join(stateDir, "xray-portal.json")
	bridgePath := filepath.Join(stateDir, "overseas-bridge.fragment.json")
	if err := writeJSON(portalPath, buildPortalConfig(cfg)); err != nil {
		return err
	}
	if err := validateXrayConfig(cfg.XrayPath, portalPath); err != nil {
		return err
	}
	if err := writeJSON(bridgePath, buildBridgeFragment(cfg)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(stateDir, "node.json"), cfg); err != nil {
		return err
	}
	fmt.Printf("Initialized reverse_node %s\n", version)
	fmt.Printf("Xray binary: %s\n", cfg.XrayPath)
	fmt.Printf("Xray Portal config: %s\n", portalPath)
	fmt.Printf("Overseas Bridge fragment: %s\n", bridgePath)
	fmt.Printf("Public endpoint: %s UDP/%d (Hysteria2, TLS+h3, force-brutal)\n", cfg.PublicHost, cfg.Port)
	fmt.Printf("Local SOCKS5: %s:%d (TCP+UDP, loopback only)\n", cfg.SocksListen, cfg.SocksPort)
	fmt.Printf("Private reverse domain: %s\n", cfg.Domain)
	if cfg.PinCertificate {
		fmt.Printf("TLS certificate SHA-256 pin: %s\n", cfg.CertificateSHA256)
		fmt.Println("The generated Bridge fragment contains this pin; keep the fragment and node.json private.")
	}
	fmt.Println("Next: merge overseas-bridge.fragment.json into the VPS Xray config without replacing existing routes/outbounds.")
	return nil
}

func checkCommand(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", defaultStateDir(), "Directory containing node.json")
	xrayPath := fs.String("xray-path", "", "Override the Xray executable path saved in node.json")
	fs.StringVar(xrayPath, "xray", "", "Alias for --xray-path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	absStateDir, err := filepath.Abs(*stateDir)
	if err != nil {
		return err
	}
	cfg, err := loadSettings(absStateDir)
	if err != nil {
		return err
	}
	if *xrayPath != "" {
		cfg.XrayPath, err = filepath.Abs(*xrayPath)
		if err != nil {
			return err
		}
	}
	if err := validateSettings(cfg); err != nil {
		return err
	}
	if err := verifyXray(cfg.XrayPath); err != nil {
		return err
	}
	if err := writeGeneratedConfigs(absStateDir, cfg); err != nil {
		return err
	}
	if err := validateXrayConfig(cfg.XrayPath, filepath.Join(absStateDir, "xray-portal.json")); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(absStateDir, "node.json"), cfg); err != nil {
		return err
	}
	fmt.Println("Xray 26.3.27 Portal configuration: OK")
	fmt.Println("The config contains a domestic Portal and a matching overseas Hysteria2 Bridge fragment.")
	return nil
}

func runCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", defaultStateDir(), "Directory containing node.json")
	xrayPath := fs.String("xray-path", "", "Override the Xray executable path saved in node.json")
	fs.StringVar(xrayPath, "xray", "", "Alias for --xray-path")
	restart := fs.Bool("restart", true, "Restart Xray if it exits unexpectedly")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	absStateDir, err := filepath.Abs(*stateDir)
	if err != nil {
		return err
	}
	cfg, err := loadSettings(absStateDir)
	if err != nil {
		return err
	}
	if *xrayPath != "" {
		cfg.XrayPath, err = filepath.Abs(*xrayPath)
		if err != nil {
			return err
		}
	}
	if err := validateSettings(cfg); err != nil {
		return err
	}
	if err := verifyXray(cfg.XrayPath); err != nil {
		return err
	}
	if err := writeGeneratedConfigs(absStateDir, cfg); err != nil {
		return err
	}
	portalPath := filepath.Join(absStateDir, "xray-portal.json")
	if err := validateXrayConfig(cfg.XrayPath, portalPath); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(absStateDir, "node.json"), cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("Starting Xray Portal with %s\n", cfg.XrayPath)
	fmt.Printf("Local SOCKS5: %s:%d | public Hysteria2: %s UDP/%d\n", cfg.SocksListen, cfg.SocksPort, cfg.PublicHost, cfg.Port)
	return superviseXray(ctx, cfg.XrayPath, portalPath, *restart)
}

func printBridgeCommand(args []string) error {
	fs := flag.NewFlagSet("print-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", defaultStateDir(), "Directory containing generated overseas-bridge.fragment.json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	path := filepath.Join(*stateDir, "overseas-bridge.fragment.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	if err == nil {
		fmt.Println()
	}
	return err
}

func loadSettings(stateDir string) (NodeSettings, error) {
	var cfg NodeSettings
	data, err := os.ReadFile(filepath.Join(stateDir, "node.json"))
	if err != nil {
		return cfg, fmt.Errorf("read node state: %w; run 'reverse_node init' first", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse node state: %w", err)
	}
	return cfg, nil
}

func writeGeneratedConfigs(stateDir string, cfg NodeSettings) error {
	if err := writeJSON(filepath.Join(stateDir, "xray-portal.json"), buildPortalConfig(cfg)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(stateDir, "overseas-bridge.fragment.json"), buildBridgeFragment(cfg)); err != nil {
		return err
	}
	return nil
}

func buildPortalConfig(cfg NodeSettings) map[string]any {
	certificate := map[string]any{"certificateFile": cfg.CertFile, "keyFile": cfg.KeyFile}
	hysteriaStream := map[string]any{
		"network":  "hysteria",
		"security": "tls",
		"tlsSettings": map[string]any{
			"alpn":         []string{"h3"},
			"certificates": []any{certificate},
		},
		"hysteriaSettings": map[string]any{"version": 2},
		"finalmask": map[string]any{
			"quicParams": map[string]any{
				"congestion": "force-brutal",
				"brutalUp":   cfg.BrutalUp,
				"brutalDown": cfg.BrutalDown,
			},
		},
	}
	return map[string]any{
		"log": map[string]any{"loglevel": cfg.LogLevel},
		"reverse": map[string]any{
			"portals": []any{map[string]any{"tag": portalTag, "domain": cfg.Domain}},
		},
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{"type": "field", "domain": []string{"full:" + cfg.Domain}, "outboundTag": portalTag},
				map[string]any{"type": "field", "inboundTag": []string{socksTag}, "outboundTag": portalTag},
			},
		},
		"inbounds": []any{
			map[string]any{
				"tag": socksTag, "listen": cfg.SocksListen, "port": cfg.SocksPort, "protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": true, "ip": cfg.SocksListen},
			},
			map[string]any{
				"tag": hysteriaInboundTag, "listen": cfg.Listen, "port": cfg.Port, "protocol": "hysteria",
				"settings":       map[string]any{"version": 2, "clients": []any{map[string]any{"auth": cfg.Auth}}},
				"streamSettings": hysteriaStream,
			},
		},
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
			map[string]any{"tag": "blocked", "protocol": "blackhole", "settings": map[string]any{}},
		},
	}
}

func buildBridgeFragment(cfg NodeSettings) map[string]any {
	tlsSettings := map[string]any{"serverName": cfg.PublicHost, "alpn": []string{"h3"}}
	if cfg.PinCertificate && cfg.CertificateSHA256 != "" {
		tlsSettings["pinnedPeerCertSha256"] = cfg.CertificateSHA256
		tlsSettings["verifyPeerCertByName"] = cfg.PublicHost
	}
	// The overseas Bridge's upload direction is the Portal's download direction.
	hysteriaStream := map[string]any{
		"network": "hysteria", "security": "tls", "tlsSettings": tlsSettings,
		"hysteriaSettings": map[string]any{"version": 2, "auth": cfg.Auth},
		"finalmask": map[string]any{
			"quicParams": map[string]any{
				"congestion": "force-brutal",
				"brutalUp":   cfg.BrutalDown,
				"brutalDown": cfg.BrutalUp,
			},
		},
	}
	return map[string]any{
		"reverse": map[string]any{
			"bridges": []any{map[string]any{"tag": bridgeTag, "domain": cfg.Domain}},
		},
		"outbounds": []any{map[string]any{
			"tag": bridgeOutboundTag, "protocol": "hysteria",
			"settings":       map[string]any{"version": 2, "address": cfg.PublicHost, "port": cfg.Port},
			"streamSettings": hysteriaStream,
		}},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{"type": "field", "domain": []string{"full:" + cfg.Domain}, "outboundTag": bridgeOutboundTag},
				map[string]any{"type": "field", "inboundTag": []string{bridgeTag}, "outboundTag": cfg.BridgeEgress},
			},
		},
	}
}

func validateSettings(cfg NodeSettings) error {
	if cfg.Version != 1 {
		return fmt.Errorf("unsupported node state version %d", cfg.Version)
	}
	if strings.TrimSpace(cfg.XrayPath) == "" {
		return errors.New("Xray executable path is empty; pass --xray-path")
	}
	if err := validatePublicHost(cfg.PublicHost); err != nil {
		return err
	}
	if net.ParseIP(cfg.Listen) == nil {
		return errors.New("--listen must be an IP address such as :: or 0.0.0.0")
	}
	if !validPort(cfg.Port) || !validPort(cfg.SocksPort) {
		return errors.New("both Hysteria2 and SOCKS5 ports must be between 1 and 65535")
	}
	if cfg.Port == cfg.SocksPort {
		return errors.New("Hysteria2 and SOCKS5 must use different port numbers because SOCKS5 uses TCP and UDP")
	}
	socksIP := net.ParseIP(cfg.SocksListen)
	if socksIP == nil || !socksIP.IsLoopback() {
		return errors.New("--socks-listen must be a loopback IP (127.0.0.1 or ::1); never expose SOCKS5 to the network")
	}
	if err := validateReverseDomain(cfg.Domain); err != nil {
		return err
	}
	if len(cfg.Auth) < 32 || len(cfg.Auth) > 1024 || strings.ContainsAny(cfg.Auth, "\r\n\t ") {
		return errors.New("Hysteria2 auth must be a strong secret of 32-1024 characters without whitespace")
	}
	if _, err := normalizeBrutalRate(cfg.BrutalUp); err != nil {
		return fmt.Errorf("invalid Brutal upload rate: %w", err)
	}
	if _, err := normalizeBrutalRate(cfg.BrutalDown); err != nil {
		return fmt.Errorf("invalid Brutal download rate: %w", err)
	}
	if strings.TrimSpace(cfg.BridgeEgress) == "" || strings.ContainsAny(cfg.BridgeEgress, "\r\n\t ") {
		return errors.New("--bridge-egress must be a non-empty Xray outbound tag without whitespace")
	}
	switch cfg.LogLevel {
	case "error", "warning", "info", "debug":
	default:
		return errors.New("--log-level must be error, warning, info or debug")
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return errors.New("TLS certificate and private key paths are required")
	}
	if cfg.PinCertificate && !sha256Pattern.MatchString(cfg.CertificateSHA256) {
		return errors.New("a pinned certificate requires a 64-character SHA-256 fingerprint")
	}
	return nil
}

func validatePublicHost(host string) error {
	if host == "" || strings.ContainsAny(host, " /\\\r\n\t") || strings.Contains(host, "://") {
		return errors.New("--public-host must be a DDNS hostname or IP address, without scheme, path or port")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	return validateDNSName(host)
}

func validateReverseDomain(domain string) error {
	if domain == "" || len(domain) > 253 || net.ParseIP(domain) != nil {
		return errors.New("--domain must be a private DNS-style name, not an IP address")
	}
	return validateDNSName(domain)
}

func validateDNSName(host string) error {
	if len(host) == 0 || len(host) > 253 {
		return fmt.Errorf("invalid DNS name %q", host)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !domainLabel.MatchString(label) {
			return fmt.Errorf("invalid DNS name %q", host)
		}
	}
	return nil
}

func validPort(port int) bool { return port > 0 && port <= 65535 }

func normalizeBrutalRate(rate string) (string, error) {
	rate = strings.ToLower(strings.TrimSpace(rate))
	if !strings.HasSuffix(rate, "mbps") {
		rate += "mbps"
	}
	if !brutalRatePattern.MatchString(rate) {
		return "", errors.New("rate must be 1-99999.99 Mbps, e.g. 16mbps")
	}
	number, err := strconv.ParseFloat(strings.TrimSuffix(rate, "mbps"), 64)
	if err != nil || number < 1 || number >= 100000 {
		return "", errors.New("rate must be between 1 and 99999.99 Mbps")
	}
	return rate, nil
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate auth token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func verifyXray(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("Xray executable %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("Xray executable path %q is a directory", path)
	}
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run Xray version: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "Xray "+xrayVersion) {
		return fmt.Errorf("Xray version mismatch: expected %s; got %s", xrayVersion, firstLine(string(out)))
	}
	return nil
}

func validateXrayConfig(xrayPath, configPath string) error {
	out, err := exec.Command(xrayPath, "run", "-test", "-c", configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Xray rejected generated Portal config: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println(strings.TrimSpace(string(out)))
	return nil
}

func superviseXray(ctx context.Context, xrayPath, configPath string, restart bool) error {
	delay := time.Second
	for {
		cmd := exec.CommandContext(ctx, xrayPath, "run", "-c", configPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			return nil
		}
		if !restart {
			if err != nil {
				return fmt.Errorf("Xray exited: %w", err)
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "Xray exited (%v); restarting in %s\n", err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay *= 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func generateSelfSignedCertificate(host, certPath, keyPath string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		_ = os.Remove(certPath)
		return err
	}
	return nil
}

func certificateSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no PEM certificate found in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func certificateTrusted(path, host string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var certs []*x509.Certificate
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return false
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{
		DNSName:       host,
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

func firstLine(value string) string {
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		return value[:index]
	}
	return strings.TrimSpace(value)
}
