package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSettings() NodeSettings {
	return NodeSettings{
		Version:           1,
		XrayPath:          "xray.exe",
		PublicHost:        "node.example.test",
		Listen:            "::",
		Port:              18443,
		SocksListen:       "127.0.0.1",
		SocksPort:         1080,
		Domain:            "reverse-node.internal",
		Auth:              "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		BrutalUp:          "16mbps",
		BrutalDown:        "24mbps",
		BridgeEgress:      "freedom",
		LogLevel:          "warning",
		CertFile:          "tls-cert.pem",
		KeyFile:           "tls-key.pem",
		PinCertificate:    true,
		CertificateSHA256: strings.Repeat("a", 64),
	}
}

func TestPortalConfigUsesDomesticPortalAndLoopbackSOCKS(t *testing.T) {
	cfg := testSettings()
	portal := buildPortalConfig(cfg)
	reverse := portal["reverse"].(map[string]any)
	portals := reverse["portals"].([]any)
	portalEntry := portals[0].(map[string]any)
	if portalEntry["tag"] != portalTag || portalEntry["domain"] != cfg.Domain {
		t.Fatalf("portal = %#v", portalEntry)
	}
	inbounds := portal["inbounds"].([]any)
	socks := inbounds[0].(map[string]any)
	if socks["listen"] != "127.0.0.1" || socks["port"] != 1080 {
		t.Fatalf("SOCKS listener is not loopback-only: %#v", socks)
	}
	if socks["tag"] != socksTag {
		t.Fatalf("SOCKS tag = %v", socks["tag"])
	}
	routing := portal["routing"].(map[string]any)
	rules := routing["rules"].([]any)
	domainRule := rules[0].(map[string]any)
	if domainRule["outboundTag"] != portalTag || domainRule["domain"].([]string)[0] != "full:"+cfg.Domain {
		t.Fatalf("reverse registration route = %#v", domainRule)
	}
	socksRule := rules[1].(map[string]any)
	if socksRule["inboundTag"].([]string)[0] != socksTag || socksRule["outboundTag"] != portalTag {
		t.Fatalf("SOCKS route = %#v", socksRule)
	}
	hysteria := inbounds[1].(map[string]any)
	if hysteria["protocol"] != "hysteria" || hysteria["port"] != cfg.Port || hysteria["listen"] != "::" {
		t.Fatalf("Hysteria listener = %#v", hysteria)
	}
	stream := hysteria["streamSettings"].(map[string]any)
	if stream["network"] != "hysteria" || stream["security"] != "tls" {
		t.Fatalf("Hysteria stream = %#v", stream)
	}
	finalmask := stream["finalmask"].(map[string]any)
	params := finalmask["quicParams"].(map[string]any)
	if params["congestion"] != "force-brutal" || params["brutalUp"] != cfg.BrutalUp || params["brutalDown"] != cfg.BrutalDown {
		t.Fatalf("Portal Brutal settings = %#v", params)
	}
}

func TestBridgeFragmentActivelyConnectsToDomesticDDNSAndReversesBrutalRates(t *testing.T) {
	cfg := testSettings()
	fragment := buildBridgeFragment(cfg)
	reverse := fragment["reverse"].(map[string]any)
	bridge := reverse["bridges"].([]any)[0].(map[string]any)
	if bridge["tag"] != bridgeTag || bridge["domain"] != cfg.Domain {
		t.Fatalf("bridge = %#v", bridge)
	}
	outbound := fragment["outbounds"].([]any)[0].(map[string]any)
	if outbound["tag"] != bridgeOutboundTag || outbound["protocol"] != "hysteria" {
		t.Fatalf("bridge outbound = %#v", outbound)
	}
	settings := outbound["settings"].(map[string]any)
	if settings["address"] != cfg.PublicHost || settings["port"] != cfg.Port {
		t.Fatalf("Bridge does not connect to domestic DDNS: %#v", settings)
	}
	stream := outbound["streamSettings"].(map[string]any)
	tls := stream["tlsSettings"].(map[string]any)
	if tls["serverName"] != cfg.PublicHost || tls["pinnedPeerCertSha256"] != cfg.CertificateSHA256 {
		t.Fatalf("Bridge TLS pin/name = %#v", tls)
	}
	finalmask := stream["finalmask"].(map[string]any)
	params := finalmask["quicParams"].(map[string]any)
	if params["congestion"] != "force-brutal" || params["brutalUp"] != cfg.BrutalDown || params["brutalDown"] != cfg.BrutalUp {
		t.Fatalf("Bridge direction-aware Brutal settings = %#v", params)
	}
	rules := fragment["routing"].(map[string]any)["rules"].([]any)
	if rules[0].(map[string]any)["outboundTag"] != bridgeOutboundTag {
		t.Fatalf("Bridge registration route must be first: %#v", rules[0])
	}
	if rules[1].(map[string]any)["outboundTag"] != cfg.BridgeEgress {
		t.Fatalf("Bridge fallback egress = %#v", rules[1])
	}
}

func TestNormalizeBrutalRate(t *testing.T) {
	for input, want := range map[string]string{
		"16": "16mbps", "16Mbps": "16mbps", "1.5mbps": "1.5mbps", "99999.99mbps": "99999.99mbps",
	} {
		got, err := normalizeBrutalRate(input)
		if err != nil || got != want {
			t.Errorf("normalizeBrutalRate(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "0.5mbps", "-1mbps", "16kbps", "100000mbps", "1.123mbps", "NaNmbps"} {
		if _, err := normalizeBrutalRate(input); err == nil {
			t.Errorf("normalizeBrutalRate(%q) accepted an invalid rate", input)
		}
	}
}

func TestValidateSettingsRejectsUnsafeSOCKSAndBadPorts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NodeSettings)
	}{
		{"public SOCKS listener", func(c *NodeSettings) { c.SocksListen = "0.0.0.0" }},
		{"same SOCKS and QUIC port", func(c *NodeSettings) { c.SocksPort = c.Port }},
		{"invalid port", func(c *NodeSettings) { c.Port = 65536 }},
		{"short auth", func(c *NodeSettings) { c.Auth = "short" }},
		{"invalid internal domain", func(c *NodeSettings) { c.Domain = "https://example.com/path" }},
		{"invalid public host", func(c *NodeSettings) { c.PublicHost = "https://example.com:443" }},
		{"invalid pin", func(c *NodeSettings) { c.CertificateSHA256 = "bad" }},
		{"invalid log level", func(c *NodeSettings) { c.LogLevel = "trace" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testSettings()
			tt.mutate(&cfg)
			if err := validateSettings(cfg); err == nil {
				t.Fatal("invalid settings were accepted")
			}
		})
	}
}

func TestGeneratedSelfSignedCertificateHasPinAndHostname(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := generateSelfSignedCertificate("node.example.test", certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	pin, err := certificateSHA256(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pin) != 64 {
		t.Fatalf("certificate SHA-256 pin has %d characters", len(pin))
	}
	if certificateTrusted(certPath, "node.example.test") {
		t.Fatal("self-signed certificate must not be treated as CA-trusted")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedConfigsAreJSONSerializable(t *testing.T) {
	cfg := testSettings()
	for name, value := range map[string]any{
		"portal": buildPortalConfig(cfg),
		"bridge": buildBridgeFragment(cfg),
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("%s config is not JSON serializable: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s config is empty", name)
		}
	}
}
