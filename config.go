package main

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"strconv"
	"strings"

	coyconf "github.com/twstrike/coyim/config"
	coyui "github.com/twstrike/coyim/ui"
	"github.com/twstrike/coyim/xmpp"
	"github.com/twstrike/otr3"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/proxy"
)

var configFile *string = flag.String("config-file", "", "Location of the config file")
var createAccount *bool = flag.Bool("create", false, "If true, attempt to create account")

func enroll(config *coyconf.Config, term *terminal.Terminal) bool {
	var err error
	warn(term, "Enrolling new config file")

	var domain string
	for {
		term.SetPrompt("Account (i.e. user@example.com, enter to quit): ")
		if config.Account, err = term.ReadLine(); err != nil || len(config.Account) == 0 {
			return false
		}

		parts := strings.SplitN(config.Account, "@", 2)
		if len(parts) != 2 {
			alert(term, "invalid username (want user@domain): "+config.Account)
			continue
		}
		domain = parts[1]
		break
	}

	term.SetPrompt("Enable debug logging to /tmp/xmpp-client-debug.log? ")
	if debugLog, err := term.ReadLine(); err != nil || !coyconf.ParseYes(debugLog) {
		info(term, "Not enabling debug logging...")
	} else {
		info(term, "Debug logging enabled...")
		config.RawLogFile = "/tmp/xmpp-client-debug.log"
	}

	term.SetPrompt("Use Tor?: ")
	if useTorQuery, err := term.ReadLine(); err != nil || len(useTorQuery) == 0 || !coyconf.ParseYes(useTorQuery) {
		info(term, "Not using Tor...")
		config.UseTor = false
	} else {
		info(term, "Using Tor...")
		config.UseTor = true
	}

	term.SetPrompt("File to import libotr private key from (enter to generate): ")

	var priv otr3.PrivateKey
	for {
		importFile, err := term.ReadLine()
		if err != nil {
			return false
		}
		if len(importFile) > 0 {
			privKeyBytes, err := ioutil.ReadFile(importFile)
			if err != nil {
				alert(term, "Failed to open private key file: "+err.Error())
				continue
			}

			if !priv.Import(privKeyBytes) {
				alert(term, "Failed to parse libotr private key file (the parser is pretty simple I'm afraid)")
				continue
			}
			break
		} else {
			info(term, "Generating private key...")
			priv.Generate(rand.Reader)
			break
		}
	}
	config.PrivateKey = priv.Serialize()

	config.OTRAutoAppendTag = true
	config.OTRAutoStartSession = true
	config.OTRAutoTearDown = false

	// List well known Tor hidden services.
	knownTorDomain := map[string]string{
		"jabber.ccc.de":             "okj7xc6j2szr2y75.onion",
		"riseup.net":                "4cjw6cwpeaeppfqz.onion",
		"jabber.calyxinstitute.org": "ijeeynrc6x2uy5ob.onion",
		"jabber.otr.im":             "5rgdtlawqkcplz75.onion",
		"wtfismyip.com":             "ofkztxcohimx34la.onion",
	}

	// Autoconfigure well known Tor hidden services.
	if hiddenService, ok := knownTorDomain[domain]; ok && config.UseTor {
		const torProxyURL = "socks5://127.0.0.1:9050"
		info(term, "It appears that you are using a well known server and we will use its Tor hidden service to connect.")
		config.Server = hiddenService
		config.Port = 5222
		config.Proxies = []string{torProxyURL}
		term.SetPrompt("> ")
		return true
	}

	var proxyStr string
	proxyDefaultPrompt := ", enter for none"
	if config.UseTor {
		proxyDefaultPrompt = ", which is the default"
	}
	term.SetPrompt("Proxy (i.e socks5://127.0.0.1:9050" + proxyDefaultPrompt + "): ")

	for {
		if proxyStr, err = term.ReadLine(); err != nil {
			return false
		}
		if len(proxyStr) == 0 {
			if !config.UseTor {
				break
			} else {
				proxyStr = "socks5://127.0.0.1:9050"
			}
		}
		u, err := url.Parse(proxyStr)
		if err != nil {
			alert(term, "Failed to parse "+proxyStr+" as a URL: "+err.Error())
			continue
		}
		if _, err = proxy.FromURL(u, proxy.Direct); err != nil {
			alert(term, "Failed to parse "+proxyStr+" as a proxy: "+err.Error())
			continue
		}
		break
	}

	if len(proxyStr) > 0 {
		config.Proxies = []string{proxyStr}

		info(term, "Since you selected a proxy, we need to know the server and port to connect to as a SRV lookup would leak information every time.")
		term.SetPrompt("Server (i.e. xmpp.example.com, enter to lookup using unproxied DNS): ")
		if config.Server, err = term.ReadLine(); err != nil {
			return false
		}
		if len(config.Server) == 0 {
			var port uint16
			info(term, "Performing SRV lookup")
			if config.Server, port, err = xmpp.Resolve(domain); err != nil {
				alert(term, "SRV lookup failed: "+err.Error())
				return false
			}
			config.Port = int(port)
			info(term, "Resolved "+config.Server+":"+strconv.Itoa(config.Port))
		} else {
			for {
				term.SetPrompt("Port (enter for 5222): ")
				portStr, err := term.ReadLine()
				if err != nil {
					return false
				}
				if len(portStr) == 0 {
					portStr = "5222"
				}
				if config.Port, err = strconv.Atoi(portStr); err != nil || config.Port <= 0 || config.Port > 65535 {
					info(term, "Port numbers must be 0 < port <= 65535")
					continue
				}
				break
			}
		}
	}

	term.SetPrompt("> ")

	return true
}

func loadConfig(ui coyui.UI) (*coyconf.Config, string, error) {
	var err error

	if len(*configFile) == 0 {
		if configFile, err = coyconf.FindConfigFile(os.Getenv("HOME")); err != nil {
			ui.Alert(err.Error())
			return nil, "", err
		}
	}

	config, err := coyconf.ParseConfig(*configFile)
	if err != nil {
		ui.Alert("Failed to parse config file: " + err.Error())
		config = new(coyconf.Config)
		if !ui.Enroll(config) {
			return config, "", errors.New("Failed to create config")
		}

		config.Filename = *configFile
		config.Save()
	}

	password := config.Password
	if len(password) == 0 {
		if password, err = ui.AskForPassword(config); err != nil {
			ui.Alert("Failed to read password: " + err.Error())
			return config, "", err
		}
	}

	return config, password, err
}

func NewXMPPConn(ui coyui.UI, config *coyconf.Config, password string, createCallback xmpp.FormCallback, logger io.Writer) (*xmpp.Conn, error) {
	parts := strings.SplitN(config.Account, "@", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid username (want user@domain): " + config.Account)
	}

	user := parts[0]
	domain := parts[1]

	var addr string
	addrTrusted := false

	if len(config.Server) > 0 && config.Port > 0 {
		addr = fmt.Sprintf("%s:%d", config.Server, config.Port)
		addrTrusted = true
	} else {
		if len(config.Proxies) > 0 {
			return nil, errors.New("Cannot connect via a proxy without Server and Port being set in the config file as an SRV lookup would leak information.")
		}

		host, port, err := xmpp.Resolve(domain)
		if err != nil {
			return nil, errors.New("Failed to resolve XMPP server: " + err.Error())
		}
		addr = fmt.Sprintf("%s:%d", host, port)
	}

	var dialer proxy.Dialer
	for i := len(config.Proxies) - 1; i >= 0; i-- {
		u, err := url.Parse(config.Proxies[i])
		if err != nil {
			return nil, errors.New("Failed to parse " + config.Proxies[i] + " as a URL: " + err.Error())
		}

		if dialer == nil {
			dialer = proxy.Direct
		}

		if dialer, err = proxy.FromURL(u, dialer); err != nil {
			return nil, errors.New("Failed to parse " + config.Proxies[i] + " as a proxy: " + err.Error())
		}
	}

	var certSHA256 []byte
	var err error
	if len(config.ServerCertificateSHA256) > 0 {
		certSHA256, err = hex.DecodeString(config.ServerCertificateSHA256)
		if err != nil {
			return nil, errors.New("Failed to parse ServerCertificateSHA256 (should be hex string): " + err.Error())
		}

		if len(certSHA256) != 32 {
			return nil, errors.New("ServerCertificateSHA256 is not 32 bytes long")
		}
	}

	xmppConfig := &xmpp.Config{
		Log:                     logger,
		CreateCallback:          createCallback,
		TrustedAddress:          addrTrusted,
		Archive:                 false,
		ServerCertificateSHA256: certSHA256,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS10,
			CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			},
		},
	}

	if domain == "jabber.ccc.de" {
		// jabber.ccc.de uses CACert but distros are removing that root
		// certificate.
		roots := x509.NewCertPool()
		caCertRoot, err := x509.ParseCertificate(caCertRootDER)
		if err == nil {
			//TODO: UI should have a Alert() method
			//alert(term, "Temporarily trusting only CACert root for CCC Jabber server")
			roots.AddCert(caCertRoot)
			xmppConfig.TLSConfig.RootCAs = roots
		} else {
			//TODO
			//alert(term, "Tried to add CACert root for jabber.ccc.de but failed: "+err.Error())
		}
	}

	//TODO: It may be locking
	//Also, move this defered functions
	//if len(config.RawLogFile) > 0 {
	//	rawLog, err := os.OpenFile(config.RawLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	//	if err != nil {
	//		return nil, errors.New("Failed to open raw log file: " + err.Error())
	//	}

	//	lock := new(sync.Mutex)
	//	in := rawLogger{
	//		out:    rawLog,
	//		prefix: []byte("<- "),
	//		lock:   lock,
	//	}
	//	out := rawLogger{
	//		out:    rawLog,
	//		prefix: []byte("-> "),
	//		lock:   lock,
	//	}
	//	in.other, out.other = &out, &in

	//	xmppConfig.InLog = &in
	//	xmppConfig.OutLog = &out

	//	defer in.flush()
	//	defer out.flush()
	//}

	if dialer != nil {
		//TODO
		//info(term, "Making connection to "+addr+" via proxy")
		if xmppConfig.Conn, err = dialer.Dial("tcp", addr); err != nil {
			return nil, errors.New("Failed to connect via proxy: " + err.Error())
		}
	}

	conn, err := xmpp.Dial(addr, user, domain, password, xmppConfig)
	if err != nil {
		return nil, errors.New("Failed to connect to XMPP server: " + err.Error())
	}

	return conn, err
}

// caCertRootDER is the DER-format, root certificate for CACert. Downloaded
// from http://www.cacert.org/certs/root.der.
var caCertRootDER = []byte{
	0x30, 0x82, 0x07, 0x3d, 0x30, 0x82, 0x05, 0x25, 0xa0, 0x03, 0x02, 0x01,
	0x02, 0x02, 0x01, 0x00, 0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86,
	0xf7, 0x0d, 0x01, 0x01, 0x04, 0x05, 0x00, 0x30, 0x79, 0x31, 0x10, 0x30,
	0x0e, 0x06, 0x03, 0x55, 0x04, 0x0a, 0x13, 0x07, 0x52, 0x6f, 0x6f, 0x74,
	0x20, 0x43, 0x41, 0x31, 0x1e, 0x30, 0x1c, 0x06, 0x03, 0x55, 0x04, 0x0b,
	0x13, 0x15, 0x68, 0x74, 0x74, 0x70, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77,
	0x2e, 0x63, 0x61, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x31,
	0x22, 0x30, 0x20, 0x06, 0x03, 0x55, 0x04, 0x03, 0x13, 0x19, 0x43, 0x41,
	0x20, 0x43, 0x65, 0x72, 0x74, 0x20, 0x53, 0x69, 0x67, 0x6e, 0x69, 0x6e,
	0x67, 0x20, 0x41, 0x75, 0x74, 0x68, 0x6f, 0x72, 0x69, 0x74, 0x79, 0x31,
	0x21, 0x30, 0x1f, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01,
	0x09, 0x01, 0x16, 0x12, 0x73, 0x75, 0x70, 0x70, 0x6f, 0x72, 0x74, 0x40,
	0x63, 0x61, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x30, 0x1e,
	0x17, 0x0d, 0x30, 0x33, 0x30, 0x33, 0x33, 0x30, 0x31, 0x32, 0x32, 0x39,
	0x34, 0x39, 0x5a, 0x17, 0x0d, 0x33, 0x33, 0x30, 0x33, 0x32, 0x39, 0x31,
	0x32, 0x32, 0x39, 0x34, 0x39, 0x5a, 0x30, 0x79, 0x31, 0x10, 0x30, 0x0e,
	0x06, 0x03, 0x55, 0x04, 0x0a, 0x13, 0x07, 0x52, 0x6f, 0x6f, 0x74, 0x20,
	0x43, 0x41, 0x31, 0x1e, 0x30, 0x1c, 0x06, 0x03, 0x55, 0x04, 0x0b, 0x13,
	0x15, 0x68, 0x74, 0x74, 0x70, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e,
	0x63, 0x61, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x31, 0x22,
	0x30, 0x20, 0x06, 0x03, 0x55, 0x04, 0x03, 0x13, 0x19, 0x43, 0x41, 0x20,
	0x43, 0x65, 0x72, 0x74, 0x20, 0x53, 0x69, 0x67, 0x6e, 0x69, 0x6e, 0x67,
	0x20, 0x41, 0x75, 0x74, 0x68, 0x6f, 0x72, 0x69, 0x74, 0x79, 0x31, 0x21,
	0x30, 0x1f, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x09,
	0x01, 0x16, 0x12, 0x73, 0x75, 0x70, 0x70, 0x6f, 0x72, 0x74, 0x40, 0x63,
	0x61, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x30, 0x82, 0x02,
	0x22, 0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01,
	0x01, 0x01, 0x05, 0x00, 0x03, 0x82, 0x02, 0x0f, 0x00, 0x30, 0x82, 0x02,
	0x0a, 0x02, 0x82, 0x02, 0x01, 0x00, 0xce, 0x22, 0xc0, 0xe2, 0x46, 0x7d,
	0xec, 0x36, 0x28, 0x07, 0x50, 0x96, 0xf2, 0xa0, 0x33, 0x40, 0x8c, 0x4b,
	0xf1, 0x3b, 0x66, 0x3f, 0x31, 0xe5, 0x6b, 0x02, 0x36, 0xdb, 0xd6, 0x7c,
	0xf6, 0xf1, 0x88, 0x8f, 0x4e, 0x77, 0x36, 0x05, 0x41, 0x95, 0xf9, 0x09,
	0xf0, 0x12, 0xcf, 0x46, 0x86, 0x73, 0x60, 0xb7, 0x6e, 0x7e, 0xe8, 0xc0,
	0x58, 0x64, 0xae, 0xcd, 0xb0, 0xad, 0x45, 0x17, 0x0c, 0x63, 0xfa, 0x67,
	0x0a, 0xe8, 0xd6, 0xd2, 0xbf, 0x3e, 0xe7, 0x98, 0xc4, 0xf0, 0x4c, 0xfa,
	0xe0, 0x03, 0xbb, 0x35, 0x5d, 0x6c, 0x21, 0xde, 0x9e, 0x20, 0xd9, 0xba,
	0xcd, 0x66, 0x32, 0x37, 0x72, 0xfa, 0xf7, 0x08, 0xf5, 0xc7, 0xcd, 0x58,
	0xc9, 0x8e, 0xe7, 0x0e, 0x5e, 0xea, 0x3e, 0xfe, 0x1c, 0xa1, 0x14, 0x0a,
	0x15, 0x6c, 0x86, 0x84, 0x5b, 0x64, 0x66, 0x2a, 0x7a, 0xa9, 0x4b, 0x53,
	0x79, 0xf5, 0x88, 0xa2, 0x7b, 0xee, 0x2f, 0x0a, 0x61, 0x2b, 0x8d, 0xb2,
	0x7e, 0x4d, 0x56, 0xa5, 0x13, 0xec, 0xea, 0xda, 0x92, 0x9e, 0xac, 0x44,
	0x41, 0x1e, 0x58, 0x60, 0x65, 0x05, 0x66, 0xf8, 0xc0, 0x44, 0xbd, 0xcb,
	0x94, 0xf7, 0x42, 0x7e, 0x0b, 0xf7, 0x65, 0x68, 0x98, 0x51, 0x05, 0xf0,
	0xf3, 0x05, 0x91, 0x04, 0x1d, 0x1b, 0x17, 0x82, 0xec, 0xc8, 0x57, 0xbb,
	0xc3, 0x6b, 0x7a, 0x88, 0xf1, 0xb0, 0x72, 0xcc, 0x25, 0x5b, 0x20, 0x91,
	0xec, 0x16, 0x02, 0x12, 0x8f, 0x32, 0xe9, 0x17, 0x18, 0x48, 0xd0, 0xc7,
	0x05, 0x2e, 0x02, 0x30, 0x42, 0xb8, 0x25, 0x9c, 0x05, 0x6b, 0x3f, 0xaa,
	0x3a, 0xa7, 0xeb, 0x53, 0x48, 0xf7, 0xe8, 0xd2, 0xb6, 0x07, 0x98, 0xdc,
	0x1b, 0xc6, 0x34, 0x7f, 0x7f, 0xc9, 0x1c, 0x82, 0x7a, 0x05, 0x58, 0x2b,
	0x08, 0x5b, 0xf3, 0x38, 0xa2, 0xab, 0x17, 0x5d, 0x66, 0xc9, 0x98, 0xd7,
	0x9e, 0x10, 0x8b, 0xa2, 0xd2, 0xdd, 0x74, 0x9a, 0xf7, 0x71, 0x0c, 0x72,
	0x60, 0xdf, 0xcd, 0x6f, 0x98, 0x33, 0x9d, 0x96, 0x34, 0x76, 0x3e, 0x24,
	0x7a, 0x92, 0xb0, 0x0e, 0x95, 0x1e, 0x6f, 0xe6, 0xa0, 0x45, 0x38, 0x47,
	0xaa, 0xd7, 0x41, 0xed, 0x4a, 0xb7, 0x12, 0xf6, 0xd7, 0x1b, 0x83, 0x8a,
	0x0f, 0x2e, 0xd8, 0x09, 0xb6, 0x59, 0xd7, 0xaa, 0x04, 0xff, 0xd2, 0x93,
	0x7d, 0x68, 0x2e, 0xdd, 0x8b, 0x4b, 0xab, 0x58, 0xba, 0x2f, 0x8d, 0xea,
	0x95, 0xa7, 0xa0, 0xc3, 0x54, 0x89, 0xa5, 0xfb, 0xdb, 0x8b, 0x51, 0x22,
	0x9d, 0xb2, 0xc3, 0xbe, 0x11, 0xbe, 0x2c, 0x91, 0x86, 0x8b, 0x96, 0x78,
	0xad, 0x20, 0xd3, 0x8a, 0x2f, 0x1a, 0x3f, 0xc6, 0xd0, 0x51, 0x65, 0x87,
	0x21, 0xb1, 0x19, 0x01, 0x65, 0x7f, 0x45, 0x1c, 0x87, 0xf5, 0x7c, 0xd0,
	0x41, 0x4c, 0x4f, 0x29, 0x98, 0x21, 0xfd, 0x33, 0x1f, 0x75, 0x0c, 0x04,
	0x51, 0xfa, 0x19, 0x77, 0xdb, 0xd4, 0x14, 0x1c, 0xee, 0x81, 0xc3, 0x1d,
	0xf5, 0x98, 0xb7, 0x69, 0x06, 0x91, 0x22, 0xdd, 0x00, 0x50, 0xcc, 0x81,
	0x31, 0xac, 0x12, 0x07, 0x7b, 0x38, 0xda, 0x68, 0x5b, 0xe6, 0x2b, 0xd4,
	0x7e, 0xc9, 0x5f, 0xad, 0xe8, 0xeb, 0x72, 0x4c, 0xf3, 0x01, 0xe5, 0x4b,
	0x20, 0xbf, 0x9a, 0xa6, 0x57, 0xca, 0x91, 0x00, 0x01, 0x8b, 0xa1, 0x75,
	0x21, 0x37, 0xb5, 0x63, 0x0d, 0x67, 0x3e, 0x46, 0x4f, 0x70, 0x20, 0x67,
	0xce, 0xc5, 0xd6, 0x59, 0xdb, 0x02, 0xe0, 0xf0, 0xd2, 0xcb, 0xcd, 0xba,
	0x62, 0xb7, 0x90, 0x41, 0xe8, 0xdd, 0x20, 0xe4, 0x29, 0xbc, 0x64, 0x29,
	0x42, 0xc8, 0x22, 0xdc, 0x78, 0x9a, 0xff, 0x43, 0xec, 0x98, 0x1b, 0x09,
	0x51, 0x4b, 0x5a, 0x5a, 0xc2, 0x71, 0xf1, 0xc4, 0xcb, 0x73, 0xa9, 0xe5,
	0xa1, 0x0b, 0x02, 0x03, 0x01, 0x00, 0x01, 0xa3, 0x82, 0x01, 0xce, 0x30,
	0x82, 0x01, 0xca, 0x30, 0x1d, 0x06, 0x03, 0x55, 0x1d, 0x0e, 0x04, 0x16,
	0x04, 0x14, 0x16, 0xb5, 0x32, 0x1b, 0xd4, 0xc7, 0xf3, 0xe0, 0xe6, 0x8e,
	0xf3, 0xbd, 0xd2, 0xb0, 0x3a, 0xee, 0xb2, 0x39, 0x18, 0xd1, 0x30, 0x81,
	0xa3, 0x06, 0x03, 0x55, 0x1d, 0x23, 0x04, 0x81, 0x9b, 0x30, 0x81, 0x98,
	0x80, 0x14, 0x16, 0xb5, 0x32, 0x1b, 0xd4, 0xc7, 0xf3, 0xe0, 0xe6, 0x8e,
	0xf3, 0xbd, 0xd2, 0xb0, 0x3a, 0xee, 0xb2, 0x39, 0x18, 0xd1, 0xa1, 0x7d,
	0xa4, 0x7b, 0x30, 0x79, 0x31, 0x10, 0x30, 0x0e, 0x06, 0x03, 0x55, 0x04,
	0x0a, 0x13, 0x07, 0x52, 0x6f, 0x6f, 0x74, 0x20, 0x43, 0x41, 0x31, 0x1e,
	0x30, 0x1c, 0x06, 0x03, 0x55, 0x04, 0x0b, 0x13, 0x15, 0x68, 0x74, 0x74,
	0x70, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e, 0x63, 0x61, 0x63, 0x65,
	0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x31, 0x22, 0x30, 0x20, 0x06, 0x03,
	0x55, 0x04, 0x03, 0x13, 0x19, 0x43, 0x41, 0x20, 0x43, 0x65, 0x72, 0x74,
	0x20, 0x53, 0x69, 0x67, 0x6e, 0x69, 0x6e, 0x67, 0x20, 0x41, 0x75, 0x74,
	0x68, 0x6f, 0x72, 0x69, 0x74, 0x79, 0x31, 0x21, 0x30, 0x1f, 0x06, 0x09,
	0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x09, 0x01, 0x16, 0x12, 0x73,
	0x75, 0x70, 0x70, 0x6f, 0x72, 0x74, 0x40, 0x63, 0x61, 0x63, 0x65, 0x72,
	0x74, 0x2e, 0x6f, 0x72, 0x67, 0x82, 0x01, 0x00, 0x30, 0x0f, 0x06, 0x03,
	0x55, 0x1d, 0x13, 0x01, 0x01, 0xff, 0x04, 0x05, 0x30, 0x03, 0x01, 0x01,
	0xff, 0x30, 0x32, 0x06, 0x03, 0x55, 0x1d, 0x1f, 0x04, 0x2b, 0x30, 0x29,
	0x30, 0x27, 0xa0, 0x25, 0xa0, 0x23, 0x86, 0x21, 0x68, 0x74, 0x74, 0x70,
	0x73, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e, 0x63, 0x61, 0x63, 0x65,
	0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x2f, 0x72, 0x65, 0x76, 0x6f, 0x6b,
	0x65, 0x2e, 0x63, 0x72, 0x6c, 0x30, 0x30, 0x06, 0x09, 0x60, 0x86, 0x48,
	0x01, 0x86, 0xf8, 0x42, 0x01, 0x04, 0x04, 0x23, 0x16, 0x21, 0x68, 0x74,
	0x74, 0x70, 0x73, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e, 0x63, 0x61,
	0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x2f, 0x72, 0x65, 0x76,
	0x6f, 0x6b, 0x65, 0x2e, 0x63, 0x72, 0x6c, 0x30, 0x34, 0x06, 0x09, 0x60,
	0x86, 0x48, 0x01, 0x86, 0xf8, 0x42, 0x01, 0x08, 0x04, 0x27, 0x16, 0x25,
	0x68, 0x74, 0x74, 0x70, 0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e, 0x63,
	0x61, 0x63, 0x65, 0x72, 0x74, 0x2e, 0x6f, 0x72, 0x67, 0x2f, 0x69, 0x6e,
	0x64, 0x65, 0x78, 0x2e, 0x70, 0x68, 0x70, 0x3f, 0x69, 0x64, 0x3d, 0x31,
	0x30, 0x30, 0x56, 0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x86, 0xf8, 0x42,
	0x01, 0x0d, 0x04, 0x49, 0x16, 0x47, 0x54, 0x6f, 0x20, 0x67, 0x65, 0x74,
	0x20, 0x79, 0x6f, 0x75, 0x72, 0x20, 0x6f, 0x77, 0x6e, 0x20, 0x63, 0x65,
	0x72, 0x74, 0x69, 0x66, 0x69, 0x63, 0x61, 0x74, 0x65, 0x20, 0x66, 0x6f,
	0x72, 0x20, 0x46, 0x52, 0x45, 0x45, 0x20, 0x68, 0x65, 0x61, 0x64, 0x20,
	0x6f, 0x76, 0x65, 0x72, 0x20, 0x74, 0x6f, 0x20, 0x68, 0x74, 0x74, 0x70,
	0x3a, 0x2f, 0x2f, 0x77, 0x77, 0x77, 0x2e, 0x63, 0x61, 0x63, 0x65, 0x72,
	0x74, 0x2e, 0x6f, 0x72, 0x67, 0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48,
	0x86, 0xf7, 0x0d, 0x01, 0x01, 0x04, 0x05, 0x00, 0x03, 0x82, 0x02, 0x01,
	0x00, 0x28, 0xc7, 0xee, 0x9c, 0x82, 0x02, 0xba, 0x5c, 0x80, 0x12, 0xca,
	0x35, 0x0a, 0x1d, 0x81, 0x6f, 0x89, 0x6a, 0x99, 0xcc, 0xf2, 0x68, 0x0f,
	0x7f, 0xa7, 0xe1, 0x8d, 0x58, 0x95, 0x3e, 0xbd, 0xf2, 0x06, 0xc3, 0x90,
	0x5a, 0xac, 0xb5, 0x60, 0xf6, 0x99, 0x43, 0x01, 0xa3, 0x88, 0x70, 0x9c,
	0x9d, 0x62, 0x9d, 0xa4, 0x87, 0xaf, 0x67, 0x58, 0x0d, 0x30, 0x36, 0x3b,
	0xe6, 0xad, 0x48, 0xd3, 0xcb, 0x74, 0x02, 0x86, 0x71, 0x3e, 0xe2, 0x2b,
	0x03, 0x68, 0xf1, 0x34, 0x62, 0x40, 0x46, 0x3b, 0x53, 0xea, 0x28, 0xf4,
	0xac, 0xfb, 0x66, 0x95, 0x53, 0x8a, 0x4d, 0x5d, 0xfd, 0x3b, 0xd9, 0x60,
	0xd7, 0xca, 0x79, 0x69, 0x3b, 0xb1, 0x65, 0x92, 0xa6, 0xc6, 0x81, 0x82,
	0x5c, 0x9c, 0xcd, 0xeb, 0x4d, 0x01, 0x8a, 0xa5, 0xdf, 0x11, 0x55, 0xaa,
	0x15, 0xca, 0x1f, 0x37, 0xc0, 0x82, 0x98, 0x70, 0x61, 0xdb, 0x6a, 0x7c,
	0x96, 0xa3, 0x8e, 0x2e, 0x54, 0x3e, 0x4f, 0x21, 0xa9, 0x90, 0xef, 0xdc,
	0x82, 0xbf, 0xdc, 0xe8, 0x45, 0xad, 0x4d, 0x90, 0x73, 0x08, 0x3c, 0x94,
	0x65, 0xb0, 0x04, 0x99, 0x76, 0x7f, 0xe2, 0xbc, 0xc2, 0x6a, 0x15, 0xaa,
	0x97, 0x04, 0x37, 0x24, 0xd8, 0x1e, 0x94, 0x4e, 0x6d, 0x0e, 0x51, 0xbe,
	0xd6, 0xc4, 0x8f, 0xca, 0x96, 0x6d, 0xf7, 0x43, 0xdf, 0xe8, 0x30, 0x65,
	0x27, 0x3b, 0x7b, 0xbb, 0x43, 0x43, 0x63, 0xc4, 0x43, 0xf7, 0xb2, 0xec,
	0x68, 0xcc, 0xe1, 0x19, 0x8e, 0x22, 0xfb, 0x98, 0xe1, 0x7b, 0x5a, 0x3e,
	0x01, 0x37, 0x3b, 0x8b, 0x08, 0xb0, 0xa2, 0xf3, 0x95, 0x4e, 0x1a, 0xcb,
	0x9b, 0xcd, 0x9a, 0xb1, 0xdb, 0xb2, 0x70, 0xf0, 0x2d, 0x4a, 0xdb, 0xd8,
	0xb0, 0xe3, 0x6f, 0x45, 0x48, 0x33, 0x12, 0xff, 0xfe, 0x3c, 0x32, 0x2a,
	0x54, 0xf7, 0xc4, 0xf7, 0x8a, 0xf0, 0x88, 0x23, 0xc2, 0x47, 0xfe, 0x64,
	0x7a, 0x71, 0xc0, 0xd1, 0x1e, 0xa6, 0x63, 0xb0, 0x07, 0x7e, 0xa4, 0x2f,
	0xd3, 0x01, 0x8f, 0xdc, 0x9f, 0x2b, 0xb6, 0xc6, 0x08, 0xa9, 0x0f, 0x93,
	0x48, 0x25, 0xfc, 0x12, 0xfd, 0x9f, 0x42, 0xdc, 0xf3, 0xc4, 0x3e, 0xf6,
	0x57, 0xb0, 0xd7, 0xdd, 0x69, 0xd1, 0x06, 0x77, 0x34, 0x0a, 0x4b, 0xd2,
	0xca, 0xa0, 0xff, 0x1c, 0xc6, 0x8c, 0xc9, 0x16, 0xbe, 0xc4, 0xcc, 0x32,
	0x37, 0x68, 0x73, 0x5f, 0x08, 0xfb, 0x51, 0xf7, 0x49, 0x53, 0x36, 0x05,
	0x0a, 0x95, 0x02, 0x4c, 0xf2, 0x79, 0x1a, 0x10, 0xf6, 0xd8, 0x3a, 0x75,
	0x9c, 0xf3, 0x1d, 0xf1, 0xa2, 0x0d, 0x70, 0x67, 0x86, 0x1b, 0xb3, 0x16,
	0xf5, 0x2f, 0xe5, 0xa4, 0xeb, 0x79, 0x86, 0xf9, 0x3d, 0x0b, 0xc2, 0x73,
	0x0b, 0xa5, 0x99, 0xac, 0x6f, 0xfc, 0x67, 0xb8, 0xe5, 0x2f, 0x0b, 0xa6,
	0x18, 0x24, 0x8d, 0x7b, 0xd1, 0x48, 0x35, 0x29, 0x18, 0x40, 0xac, 0x93,
	0x60, 0xe1, 0x96, 0x86, 0x50, 0xb4, 0x7a, 0x59, 0xd8, 0x8f, 0x21, 0x0b,
	0x9f, 0xcf, 0x82, 0x91, 0xc6, 0x3b, 0xbf, 0x6b, 0xdc, 0x07, 0x91, 0xb9,
	0x97, 0x56, 0x23, 0xaa, 0xb6, 0x6c, 0x94, 0xc6, 0x48, 0x06, 0x3c, 0xe4,
	0xce, 0x4e, 0xaa, 0xe4, 0xf6, 0x2f, 0x09, 0xdc, 0x53, 0x6f, 0x2e, 0xfc,
	0x74, 0xeb, 0x3a, 0x63, 0x99, 0xc2, 0xa6, 0xac, 0x89, 0xbc, 0xa7, 0xb2,
	0x44, 0xa0, 0x0d, 0x8a, 0x10, 0xe3, 0x6c, 0xf2, 0x24, 0xcb, 0xfa, 0x9b,
	0x9f, 0x70, 0x47, 0x2e, 0xde, 0x14, 0x8b, 0xd4, 0xb2, 0x20, 0x09, 0x96,
	0xa2, 0x64, 0xf1, 0x24, 0x1c, 0xdc, 0xa1, 0x35, 0x9c, 0x15, 0xb2, 0xd4,
	0xbc, 0x55, 0x2e, 0x7d, 0x06, 0xf5, 0x9c, 0x0e, 0x55, 0xf4, 0x5a, 0xd6,
	0x93, 0xda, 0x76, 0xad, 0x25, 0x73, 0x4c, 0xc5, 0x43,
}
