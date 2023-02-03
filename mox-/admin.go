package mox

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mjl-/mox/config"
	"github.com/mjl-/mox/dkim"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/junk"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mtasts"
	"github.com/mjl-/mox/smtp"
)

// TXTStrings returns a TXT record value as one or more quoted strings, taking the max
// length of 255 characters for a string into account.
func TXTStrings(s string) string {
	r := ""
	for len(s) > 0 {
		n := len(s)
		if n > 255 {
			n = 255
		}
		if r != "" {
			r += " "
		}
		r += `"` + s[:n] + `"`
		s = s[n:]
	}
	return r
}

// MakeDKIMEd25519Key returns a PEM buffer containing an ed25519 key for use
// with DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMEd25519Key(selector, domain dns.Domain) ([]byte, error) {
	_, privKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("ed25519", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

func dkimKeyNote(kind string, selector, domain dns.Domain) string {
	s := kind + " dkim private key"
	var zero dns.Domain
	if selector != zero && domain != zero {
		s += fmt.Sprintf(" for %s._domainkey.%s", selector.ASCII, domain.ASCII)
	}
	s += fmt.Sprintf(", generated by mox on %s", time.Now().Format(time.RFC3339))
	return s
}

// MakeDKIMEd25519Key returns a PEM buffer containing an rsa key for use with
// DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMRSAKey(selector, domain dns.Domain) ([]byte, error) {
	// 2048 bits seems reasonable in 2022, 1024 is on the low side, larger
	// keys may not fit in UDP DNS response.
	privKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("rsa", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

// MakeAccountConfig returns a new account configuration for an email address.
func MakeAccountConfig(addr smtp.Address) config.Account {
	account := config.Account{
		Domain: addr.Domain.Name(),
		Destinations: map[string]config.Destination{
			addr.Localpart.String(): {},
		},
		RejectsMailbox: "Rejects",
		JunkFilter: &config.JunkFilter{
			Threshold: 0.95,
			Params: junk.Params{
				Onegrams:    true,
				MaxPower:    .01,
				TopWords:    10,
				IgnoreWords: .1,
				RareWords:   2,
			},
		},
	}
	account.SubjectPass.Period = 12 * time.Hour
	return account
}

// MakeDomainConfig makes a new config for a domain, creating DKIM keys, using
// accountName for DMARC and TLS reports.
func MakeDomainConfig(ctx context.Context, domain, hostname dns.Domain, accountName string) (config.Domain, []string, error) {
	log := xlog.WithContext(ctx)

	now := time.Now()
	year := now.Format("2006")
	timestamp := now.Format("20060102T150405")

	var paths []string
	defer func() {
		for _, p := range paths {
			if err := os.Remove(p); err != nil {
				log.Errorx("removing path for domain config", err, mlog.Field("path", p))
			}
		}
	}()

	writeFile := func(path string, data []byte) error {
		os.MkdirAll(filepath.Dir(path), 0770)

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0660)
		if err != nil {
			return fmt.Errorf("creating file %s: %s", path, err)
		}
		defer func() {
			if f != nil {
				os.Remove(path)
				f.Close()
			}
		}()
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("writing file %s: %s", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close file: %v", err)
		}
		f = nil
		return nil
	}

	confDKIM := config.DKIM{
		Selectors: map[string]config.Selector{},
	}

	addSelector := func(kind, name string, privKey []byte) error {
		record := fmt.Sprintf("%s._domainkey.%s", name, domain.ASCII)
		keyPath := filepath.Join("dkim", fmt.Sprintf("%s.%s.%skey.pkcs8.pem", record, timestamp, kind))
		p := ConfigDirPath(keyPath)
		if err := writeFile(p, privKey); err != nil {
			return err
		}
		paths = append(paths, p)
		confDKIM.Selectors[name] = config.Selector{
			// Example from RFC has 5 day between signing and expiration. ../rfc/6376:1393
			// Expiration is not intended as antireplay defense, but it may help. ../rfc/6376:1340
			// Messages in the wild have been observed with 2 hours and 1 year expiration.
			Expiration:     "72h",
			PrivateKeyFile: keyPath,
		}
		return nil
	}

	addEd25519 := func(name string) error {
		key, err := MakeDKIMEd25519Key(dns.Domain{ASCII: name}, domain)
		if err != nil {
			return fmt.Errorf("making dkim ed25519 private key: %s", err)
		}
		return addSelector("ed25519", name, key)
	}

	addRSA := func(name string) error {
		key, err := MakeDKIMRSAKey(dns.Domain{ASCII: name}, domain)
		if err != nil {
			return fmt.Errorf("making dkim rsa private key: %s", err)
		}
		return addSelector("rsa", name, key)
	}

	if err := addEd25519(year + "a"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addRSA(year + "b"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addEd25519(year + "c"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addRSA(year + "d"); err != nil {
		return config.Domain{}, nil, err
	}

	// We sign with the first two. In case they are misused, the switch to the other
	// keys is easy, just change the config. Operators should make the public key field
	// of the misused keys empty in the DNS records to disable the misused keys.
	confDKIM.Sign = []string{year + "a", year + "b"}

	confDomain := config.Domain{
		LocalpartCatchallSeparator: "+",
		DKIM:                       confDKIM,
		DMARC: &config.DMARC{
			Account:   accountName,
			Localpart: "dmarc-reports",
			Mailbox:   "DMARC",
		},
		MTASTS: &config.MTASTS{
			PolicyID: time.Now().UTC().Format("20060102T150405"),
			Mode:     mtasts.ModeEnforce,
			// We start out with 24 hour, and warn in the admin interface that users should
			// increase it to weeks. Once the setup works.
			MaxAge: 24 * time.Hour,
			MX:     []string{hostname.ASCII},
		},
		TLSRPT: &config.TLSRPT{
			Account:   accountName,
			Localpart: "tls-reports",
			Mailbox:   "TLSRPT",
		},
	}

	rpaths := paths
	paths = nil

	return confDomain, rpaths, nil
}

// DomainAdd adds the domain to the domains config, rewriting domains.conf and
// marking it loaded.
//
// accountName is used for DMARC/TLS report.
// If the account does not exist, it is created with localpart. Localpart must be
// set only if the account does not yet exist.
func DomainAdd(ctx context.Context, domain dns.Domain, accountName string, localpart smtp.Localpart) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding domain", rerr, mlog.Field("domain", domain), mlog.Field("account", accountName), mlog.Field("localpart", localpart))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Domains[domain.Name()]; ok {
		return fmt.Errorf("domain already present")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	for name, d := range c.Domains {
		nc.Domains[name] = d
	}

	confDomain, cleanupFiles, err := MakeDomainConfig(ctx, domain, Conf.Static.HostnameDomain, accountName)
	if err != nil {
		return fmt.Errorf("preparing domain config: %v", err)
	}
	defer func() {
		for _, f := range cleanupFiles {
			if err := os.Remove(f); err != nil {
				log.Errorx("cleaning up file after error", err, mlog.Field("path", f))
			}
		}
	}()

	if _, ok := c.Accounts[accountName]; ok && localpart != "" {
		return fmt.Errorf("account already exists (leave localpart empty when using an existing account)")
	} else if !ok && localpart == "" {
		return fmt.Errorf("account does not yet exist (specify a localpart)")
	} else if accountName == "" {
		return fmt.Errorf("account name is empty")
	} else if !ok {
		nc.Accounts[accountName] = MakeAccountConfig(smtp.Address{Localpart: localpart, Domain: domain})
	}

	nc.Domains[domain.Name()] = confDomain

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("domain added", mlog.Field("domain", domain))
	cleanupFiles = nil // All good, don't cleanup.
	return nil
}

// DomainRemove removes domain from the config, rewriting domains.conf.
//
// No accounts are removed, also not when they still reference this domain.
func DomainRemove(ctx context.Context, domain dns.Domain) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing domain", rerr, mlog.Field("domain", domain))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	domConf, ok := c.Domains[domain.Name()]
	if !ok {
		return fmt.Errorf("domain does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	s := domain.Name()
	for name, d := range c.Domains {
		if name != s {
			nc.Domains[name] = d
		}
	}

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}

	// Move away any DKIM private keys to a subdirectory "old". But only if
	// they are not in use by other domains.
	usedKeyPaths := map[string]bool{}
	for _, dc := range nc.Domains {
		for _, sel := range dc.DKIM.Selectors {
			usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] = true
		}
	}
	for _, sel := range domConf.DKIM.Selectors {
		if sel.PrivateKeyFile == "" || usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] {
			continue
		}
		src := ConfigDirPath(sel.PrivateKeyFile)
		dst := ConfigDirPath(filepath.Join(filepath.Dir(sel.PrivateKeyFile), "old", filepath.Base(sel.PrivateKeyFile)))
		_, err := os.Stat(dst)
		if err == nil {
			err = fmt.Errorf("destination already exists")
		} else if os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(dst), 0770)
			err = os.Rename(src, dst)
		}
		if err != nil {
			log.Errorx("renaming dkim private key file for removed domain", err, mlog.Field("src", src), mlog.Field("dst", dst))
		}
	}

	log.Info("domain removed", mlog.Field("domain", domain))
	return nil
}

// todo: find a way to automatically create the dns records as it would greatly simplify setting up email for a domain. we could also dynamically make changes, e.g. providing grace periods after disabling a dkim key, only automatically removing the dkim dns key after a few days. but this requires some kind of api and authentication to the dns server. there doesn't appear to be a single commonly used api for dns management. each of the numerous cloud providers have their own APIs and rather large SKDs to use them. we don't want to link all of them in.

// DomainRecords returns text lines describing DNS records required for configuring
// a domain.
func DomainRecords(domConf config.Domain, domain dns.Domain) ([]string, error) {
	d := domain.ASCII
	h := Conf.Static.HostnameDomain.ASCII

	records := []string{
		"; Time To Live, may be recognized if importing as a zone file.",
		"$TTL 300",
		"",

		"; For the machine, only needs to be created for the first domain added.",
		fmt.Sprintf(`%-*s IN TXT "v=spf1 a -all"`, 20+len(d), h+"."), // ../rfc/7208:2263 ../rfc/7208:2287
		"",

		"; Deliver email for the domain to this host.",
		fmt.Sprintf("%s.                    MX 10 %s.", d, h),
		"",

		"; Outgoing messages will be signed with the first two DKIM keys. The other two",
		"; configured for backup, switching to them is just a config change.",
	}
	var selectors []string
	for name := range domConf.DKIM.Selectors {
		selectors = append(selectors, name)
	}
	sort.Slice(selectors, func(i, j int) bool {
		return selectors[i] < selectors[j]
	})
	for _, name := range selectors {
		sel := domConf.DKIM.Selectors[name]
		dkimr := dkim.Record{
			Version:   "DKIM1",
			Hashes:    []string{"sha256"},
			PublicKey: sel.Key.Public(),
		}
		if _, ok := sel.Key.(ed25519.PrivateKey); ok {
			dkimr.Key = "ed25519"
		} else if _, ok := sel.Key.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("unrecognized private key for DKIM selector %q: %T", name, sel.Key)
		}
		txt, err := dkimr.Record()
		if err != nil {
			return nil, fmt.Errorf("making DKIM DNS TXT record: %v", err)
		}

		if len(txt) > 255 {
			records = append(records,
				"; NOTE: Ensure the next record is added in DNS as a single record, it consists",
				"; of multiple strings (max size of each is 255 bytes).",
			)
		}
		s := fmt.Sprintf("%s._domainkey.%s.   IN TXT %s", name, d, TXTStrings(txt))
		records = append(records, s)

	}
	records = append(records,
		"",

		"; Specify the MX host is allowed to send for our domain and for itself (for DSNs).",
		"; ~all means softfail for anything else, which is done instead of -all to prevent older",
		"; mail servers from rejecting the message because they never get to looking for a dkim/dmarc pass.",
		fmt.Sprintf(`%s.                    IN TXT "v=spf1 mx ~all"`, d),
		"",

		"; Emails that fail the DMARC check (without DKIM and without SPF) should be rejected, and request reports.",
		"; If you email through mailing lists that strip DKIM-Signature headers and don't",
		"; rewrite the From header, you may want to set the policy to p=none.",
		fmt.Sprintf(`_dmarc.%s.             IN TXT "v=DMARC1; p=reject; rua=mailto:dmarc-reports@%s!10m"`, d, d),
		"",
	)

	if sts := domConf.MTASTS; sts != nil {
		records = append(records,
			"; TLS must be used when delivering to us.",
			fmt.Sprintf(`mta-sts.%s.            IN CNAME %s.`, d, h),
			fmt.Sprintf(`_mta-sts.%s.           IN TXT "v=STSv1; id=%s"`, d, sts.PolicyID),
			"",
		)
	}

	records = append(records,
		"; Request reporting about TLS failures.",
		fmt.Sprintf(`_smtp._tls.%s.         IN TXT "v=TLSRPTv1; rua=mailto:tls-reports@%s"`, d, d),
		"",

		"; Autoconfig is used by Thunderbird. Autodiscover is (in theory) used by Microsoft.",
		fmt.Sprintf(`autoconfig.%s.         IN CNAME %s.`, d, h),
		fmt.Sprintf(`_autodiscover._tcp.%s. IN SRV 0 1 443 autoconfig.%s.`, d, d),
		"",

		// ../rfc/6186:133 ../rfc/8314:692
		"; For secure IMAP and submission autoconfig, point to mail host.",
		fmt.Sprintf(`_imaps._tcp.%s.        IN SRV 0 1 993 %s.`, d, h),
		fmt.Sprintf(`_submissions._tcp.%s.  IN SRV 0 1 465 %s.`, d, h),
		"",
		// ../rfc/6186:242
		"; Next records specify POP3 and plain text ports are not to be used.",
		fmt.Sprintf(`_imap._tcp.%s.         IN SRV 0 1 143 .`, d),
		fmt.Sprintf(`_submission._tcp.%s.   IN SRV 0 1 587 .`, d),
		fmt.Sprintf(`_pop3._tcp.%s.         IN SRV 0 1 110 .`, d),
		fmt.Sprintf(`_pop3s._tcp.%s.        IN SRV 0 1 995 .`, d),
		"",

		"; Optional:",
		"; You could mark Let's Encrypt as the only Certificate Authority allowed to",
		"; sign TLS certificates for your domain.",
		fmt.Sprintf("%s.                    IN CAA 0 issue \"letsencrypt.org\"", d),
	)
	return records, nil
}

// AccountAdd adds an account and an initial address and reloads the
// configuration.
//
// The new account does not have a password, so cannot log in. Email can be
// delivered.
func AccountAdd(ctx context.Context, account, address string) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, mlog.Field("account", account), mlog.Field("address", address))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Accounts[account]; ok {
		return fmt.Errorf("account already present")
	}

	addr, err := smtp.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("parsing email address: %v", err)
	}
	if _, ok := Conf.accountDestinations[addr.String()]; ok {
		return fmt.Errorf("address already exists")
	}

	dname := addr.Domain.Name()
	if _, ok := c.Domains[dname]; !ok {
		return fmt.Errorf("domain does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[account] = MakeAccountConfig(addr)

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account added", mlog.Field("account", account), mlog.Field("address", addr))
	return nil
}

// AccountRemove removes an account and reloads the configuration.
func AccountRemove(ctx context.Context, account string) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, mlog.Field("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Accounts[account]; !ok {
		return fmt.Errorf("account does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		if name != account {
			nc.Accounts[name] = a
		}
	}

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account removed", mlog.Field("account", account))
	return nil
}

// AddressAdd adds an email address to an account and reloads the
// configuration.
func AddressAdd(ctx context.Context, address, account string) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding address", rerr, mlog.Field("address", address), mlog.Field("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	a, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("account does not exist")
	}

	addr, err := smtp.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("parsing email address: %v", err)
	}
	if _, ok := Conf.accountDestinations[addr.String()]; ok {
		return fmt.Errorf("address already exists")
	}

	dname := addr.Domain.Name()
	if _, ok := c.Domains[dname]; !ok {
		return fmt.Errorf("domain does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nd := map[string]config.Destination{}
	for name, d := range a.Destinations {
		nd[name] = d
	}
	var k string
	if addr.Domain == a.DNSDomain {
		k = addr.Localpart.String()
	} else {
		k = addr.String()
	}
	nd[k] = config.Destination{}
	a.Destinations = nd
	nc.Accounts[account] = a

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("address added", mlog.Field("address", addr), mlog.Field("account", account))
	return nil
}

// AddressRemove removes an email address and reloads the configuration.
func AddressRemove(ctx context.Context, address string) (rerr error) {
	log := xlog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing address", rerr, mlog.Field("address", address))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic

	addr, err := smtp.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("parsing email address: %v", err)
	}
	ad, ok := Conf.accountDestinations[addr.String()]
	if !ok {
		return fmt.Errorf("address does not exists")
	}
	addrStr := addr.String()

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	a, ok := c.Accounts[ad.Account]
	if !ok {
		return fmt.Errorf("internal error: cannot find account")
	}
	na := a
	na.Destinations = map[string]config.Destination{}
	var dropped bool
	for name, d := range a.Destinations {
		if !(name == addr.Localpart.String() && a.DNSDomain == addr.Domain || name == addrStr) {
			na.Destinations[name] = d
		} else {
			dropped = true
		}
	}
	if !dropped {
		return fmt.Errorf("address not removed, likely a postmaster/reporting address")
	}
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[ad.Account] = na

	if err := writeDynamic(ctx, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("address removed", mlog.Field("address", addr), mlog.Field("account", ad.Account))
	return nil
}

// ClientConfig holds the client configuration for IMAP/Submission for a
// domain.
type ClientConfig struct {
	Entries []ClientConfigEntry
}

type ClientConfigEntry struct {
	Protocol string
	Host     dns.Domain
	Port     int
	Listener string
	Note     string
}

// ClientConfigDomain returns the client config for IMAP/Submission for a
// domain.
func ClientConfigDomain(d dns.Domain) (ClientConfig, error) {
	_, ok := Conf.Domain(d)
	if !ok {
		return ClientConfig{}, fmt.Errorf("unknown domain")
	}

	c := ClientConfig{}
	c.Entries = []ClientConfigEntry{}
	var listeners []string

	for name := range Conf.Static.Listeners {
		listeners = append(listeners, name)
	}
	sort.Slice(listeners, func(i, j int) bool {
		return listeners[i] < listeners[j]
	})

	note := func(tls bool, requiretls bool) string {
		if !tls {
			return "plain text, no STARTTLS configured"
		}
		if requiretls {
			return "STARTTLS required"
		}
		return "STARTTLS optional"
	}

	for _, name := range listeners {
		l := Conf.Static.Listeners[name]
		host := Conf.Static.HostnameDomain
		if l.Hostname != "" {
			host = l.HostnameDomain
		}
		if l.Submissions.Enabled {
			c.Entries = append(c.Entries, ClientConfigEntry{"Submission (SMTP)", host, config.Port(l.Submissions.Port, 465), name, "with TLS"})
		}
		if l.IMAPS.Enabled {
			c.Entries = append(c.Entries, ClientConfigEntry{"IMAP", host, config.Port(l.IMAPS.Port, 993), name, "with TLS"})
		}
		if l.Submission.Enabled {
			c.Entries = append(c.Entries, ClientConfigEntry{"Submission (SMTP)", host, config.Port(l.Submission.Port, 587), name, note(l.TLS != nil, !l.Submission.NoRequireSTARTTLS)})
		}
		if l.IMAP.Enabled {
			c.Entries = append(c.Entries, ClientConfigEntry{"IMAP", host, config.Port(l.IMAPS.Port, 143), name, note(l.TLS != nil, !l.IMAP.NoRequireSTARTTLS)})
		}
	}

	return c, nil
}

// return IPs we may be listening on or connecting from to the outside.
func IPs(ctx context.Context) ([]net.IP, error) {
	log := xlog.WithContext(ctx)

	// Try to gather all IPs we are listening on by going through the config.
	// If we encounter 0.0.0.0 or ::, we'll gather all local IPs afterwards.
	var ips []net.IP
	var ipv4all, ipv6all bool
	for _, l := range Conf.Static.Listeners {
		for _, s := range l.IPs {
			ip := net.ParseIP(s)
			if ip.IsUnspecified() {
				if ip.To4() != nil {
					ipv4all = true
				} else {
					ipv6all = true
				}
				continue
			}
			ips = append(ips, ip)
		}
	}

	// We'll list the IPs on the interfaces. How useful is this? There is a good chance
	// we're listening on all addresses because of a load balancing/firewall.
	if ipv4all || ipv6all {
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("listing network interfaces: %v", err)
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				return nil, fmt.Errorf("listing addresses for network interface: %v", err)
			}
			if len(addrs) == 0 {
				continue
			}

			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil {
					log.Errorx("bad interface addr", err, mlog.Field("address", addr))
					continue
				}
				v4 := ip.To4() != nil
				if ipv4all && v4 || ipv6all && !v4 {
					ips = append(ips, ip)
				}
			}
		}
	}
	return ips, nil
}
