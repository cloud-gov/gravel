package dns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/18f/gravel/wrappers"
	"github.com/go-acme/lego/v3/challenge"
	"github.com/go-acme/lego/v3/challenge/dns01"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

type IntegrationServer struct {
	// RecordsHandler that can be queried against.
	TestRecords map[string]string

	// Records received and available for downstream processing.
	Records []DnsMessage

	// Channel for receiving records. Mostly used for internal purposes.
	RecordsHandler chan DnsMessage

	// Options on how the DNS instance is running.
	Opts *IntegrationServerOpts

	// The base DNS instance.
	Server *dns.Server

	// Stop the DNS Server
	Stopper chan struct{}

	// Logger
	logger *logrus.Logger

	// testing reference.
	t *wrappers.TestWrapper

	// internal locking for safety.
	mu sync.RWMutex
}

// Helpful testing options.
type IntegrationServerOpts struct {
	// Set to true if you want records automatically generated by Gravel to be added to the DNS server for automatic
	// verification.
	AutoUpdateAuthZRecords bool

	// Set to false if records being added to the server are already encrypted. Defaults to false.
	AlreadyHashed bool

	// The records handler so the DNS server can interact update records.
	RecordHandler chan DnsMessage

	// The base domain you want to test with. For example, if you are testing with `test.service`, your base domain
	// would be "service"
	BaseDomain string

	// Reserved:
	//   53: localhost
	//   5353: mDNS on macos
	// Defaults to 5454 due to lack of setcap on macos.
	DnsPort int

	// Logger
	Logger *logrus.Logger

	// DNS Challenge provider
	Provider challenge.Provider
}

func NewDefaultIntegrationServerOpts() *IntegrationServerOpts {
	rh := make(chan DnsMessage, 10)
	return &IntegrationServerOpts{
		AutoUpdateAuthZRecords: true,
		RecordHandler:          rh,
		BaseDomain:             "service",
		DnsPort:                5454,
		Logger:                 logrus.New(),
		Provider:               NewDnsProvider(rh),
		AlreadyHashed:          false,
	}
}

// Generate a new DNS integration server configuration.
func NewIntegrationServer(opts *IntegrationServerOpts) *IntegrationServer {
	is := &IntegrationServer{}

	is.Opts = opts
	is.RecordsHandler = opts.RecordHandler
	is.Records = make([]DnsMessage, 0)
	is.TestRecords = make(map[string]string)
	if is.Opts.DnsPort == 0 {
		is.Opts.DnsPort = 54
	}

	is.Stopper = make(chan struct{}, 1)
	is.logger = opts.Logger

	return is
}

// Start the DNS integration.
func (is *IntegrationServer) Start() {
	// todo (mxplusb): use a channel for sharing errors and statuses.

	// attach request handler func
	dns.HandleFunc(is.Opts.BaseDomain+".", is.handleDnsRequest)

	// start server
	is.Server = &dns.Server{Addr: ":" + strconv.Itoa(is.Opts.DnsPort), Net: "udp"}

	// start our background record updater and stop handler.
	go is.handleRecords()
	go is.stopHandler()

	err := is.Server.ListenAndServe()
	if err != nil {
		panic(err)
	}
}

func (is *IntegrationServer) PreCheck(domain, fqdn, value string, check dns01.PreCheckFunc) (b bool, e error) {

	dnsClient := dns.Client{}
	msg := &dns.Msg{}
	msg.SetQuestion(fqdn, dns.TypeTXT)

	reply, _, err := dnsClient.Exchange(msg, is.Server.Addr)
	if err != nil {
		return false, err
	}

	if t, ok := reply.Answer[0].(*dns.TXT); ok {
		// if the txt record resolves as intended, mark this resolver as true.
		for idx := range t.Txt {
			if t.Txt[idx] == value {
				return true, nil
			}
		}
	}

	return false, nil
}

// Backgrounder for stopping the server.
func (is *IntegrationServer) stopHandler() {
	ctx := context.TODO()
	for {
		select {
		case _ = <-is.Stopper:
			lctx, _ := context.WithTimeout(ctx, time.Second*2)
			err := is.Server.ShutdownContext(lctx)
			if err != nil {
				is.t.Error(err)
			}
		}
	}
}

// Handler for updating the DNS server with new records.
func (is *IntegrationServer) handleRecords() {
	for {
		select {
		case msg := <-is.RecordsHandler:
			is.Records = append(is.Records, msg)
			if is.Opts.AutoUpdateAuthZRecords {
				is.mu.Lock()

				var value string
				if !is.Opts.AlreadyHashed {
					keyAuthShaBytes := sha256.Sum256([]byte(msg.KeyAuth))
					value = base64.RawURLEncoding.EncodeToString(keyAuthShaBytes[:sha256.Size])
				} else {
					value = msg.KeyAuth
				}

				is.TestRecords["_acme-challenge."+msg.Domain+"."] = value
				is.mu.Unlock()
			}
		}
	}
}

// handle incoming dns queries.
func (is *IntegrationServer) parseQuery(m *dns.Msg) {
	for _, q := range m.Question {
		switch q.Qtype {
		case dns.TypeTXT:
			txt := is.TestRecords[q.Name]
			if txt != "" {
				rr, _ := dns.NewRR(fmt.Sprintf("%s 60 IN TXT %s", q.Name, txt))
				m.Answer = append(m.Answer, rr)
			}
		}
	}
}

func (is *IntegrationServer) handleDnsRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false

	switch r.Opcode {
	case dns.OpcodeQuery:
		is.parseQuery(m)
	}

	w.WriteMsg(m)
}
