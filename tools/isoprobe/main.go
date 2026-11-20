// Command isoprobe checks a container's network isolation from the inside.
//
// Each flag names one thing that must work or must fail. The probe prints
// one line per check and exits with status 1 if any check went the wrong way.
// It is a test tool for the EZBox isolation design, not part of a box.
//
//	isoprobe -allow-http http://gateway:8000/v1/models \
//	         -scan gateway -open gateway:8000 \
//	         -deny-tcp 1.1.1.1:443 -deny-name example.com -deny-dns 8.8.8.8
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// probe holds the settings shared by every check.
type probe struct {
	timeout time.Duration
	out     io.Writer
	failed  bool
}

func (p *probe) report(ok bool, check, target, detail string) {
	word := "PASS"
	if !ok {
		word = "FAIL"
		p.failed = true
	}
	fmt.Fprintf(p.out, "%s  %-10s  %-40s  %s\n", word, check, target, detail)
}

func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("isoprobe", flag.ContinueOnError)
	fs.SetOutput(out)
	var allowHTTP, denyTCP, denyName, denyDNS, scan, open, resolve list
	fs.Var(&allowHTTP, "allow-http", "URL that must answer with status 200 (repeatable)")
	fs.Var(&denyTCP, "deny-tcp", "host:port that must not accept a TCP connection (repeatable)")
	fs.Var(&denyName, "deny-name", "name that must not resolve (repeatable)")
	fs.Var(&denyDNS, "deny-dns", "IP address of a DNS server that must not answer (repeatable)")
	fs.Var(&scan, "scan", "host whose TCP ports must all be closed, except those given with -open (repeatable)")
	fs.Var(&open, "open", "host:port that -scan expects to be open (repeatable)")
	fs.Var(&resolve, "resolve", "name to resolve and print, with no pass or fail (repeatable)")
	serve := fs.String("serve", "", "listen on this TCP address forever, as a target for other probes")
	timeout := fs.Duration("timeout", 2*time.Second, "time limit for each connection or lookup")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *serve != "" {
		return serveForever(*serve, out)
	}

	p := &probe{timeout: *timeout, out: out}
	for _, name := range resolve {
		addrs, err := p.lookup(name)
		if err != nil {
			fmt.Fprintf(out, "INFO  resolve     %-40s  %v\n", name, err)
		} else {
			fmt.Fprintf(out, "INFO  resolve     %-40s  %s\n", name, strings.Join(addrs, " "))
		}
	}
	for _, u := range allowHTTP {
		p.allowHTTP(u)
	}
	for _, name := range denyName {
		addrs, err := p.lookup(name)
		p.report(err != nil, "deny-name", name, describe(addrs, err))
	}
	for _, ip := range denyDNS {
		p.denyDNS(ip)
	}
	for _, hp := range denyTCP {
		err := p.dial(hp)
		p.report(err != nil, "deny-tcp", hp, describe(nil, err))
	}
	for _, host := range scan {
		p.scan(host, open)
	}
	if p.failed {
		return 1
	}
	return 0
}

func describe(addrs []string, err error) string {
	if err != nil {
		return "blocked: " + shortErr(err)
	}
	if len(addrs) > 0 {
		return "reachable: " + strings.Join(addrs, " ")
	}
	return "reachable"
}

// shortErr keeps the end of an error, which says what actually happened.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

func (p *probe) lookup(name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, name)
}

func (p *probe) dial(hostport string) error {
	c, err := net.DialTimeout("tcp", hostport, p.timeout)
	if err == nil {
		c.Close()
	}
	return err
}

func (p *probe) allowHTTP(u string) {
	client := &http.Client{Timeout: p.timeout * 5}
	resp, err := client.Get(u)
	if err != nil {
		p.report(false, "allow-http", u, "failed: "+shortErr(err))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	p.report(resp.StatusCode == http.StatusOK, "allow-http", u, "status "+strconv.Itoa(resp.StatusCode))
}

// denyDNS sends one plain DNS question straight to a server, bypassing the
// container's resolver. Any answer at all means the server was reachable.
func (p *probe) denyDNS(ip string) {
	target := net.JoinHostPort(ip, "53")
	c, err := net.DialTimeout("udp", target, p.timeout)
	if err != nil {
		p.report(true, "deny-dns", target, "blocked: "+shortErr(err))
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(p.timeout))
	if _, err := c.Write(dnsQuery("example.com")); err != nil {
		p.report(true, "deny-dns", target, "blocked: "+shortErr(err))
		return
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		p.report(true, "deny-dns", target, "blocked: "+shortErr(err))
		return
	}
	p.report(false, "deny-dns", target, "answered with "+strconv.Itoa(n)+" bytes")
}

// dnsQuery builds a minimal DNS question for the A record of name.
func dnsQuery(name string) []byte {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	return append(q, 0, 0, 1, 0, 1)
}

// scan tries every TCP port on host and checks that exactly the expected
// ones are open. An expected port that is closed is also a failure, so a
// scan that reaches nothing at all can't pass by accident.
func (p *probe) scan(host string, open list) {
	want := map[int]bool{}
	for _, hp := range open {
		h, port, err := net.SplitHostPort(hp)
		if err != nil || h != host {
			continue
		}
		if n, err := strconv.Atoi(port); err == nil {
			want[n] = true
		}
	}

	ports := make(chan int)
	var mu sync.Mutex
	var found []int
	var wg sync.WaitGroup
	for w := 0; w < 256; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for port := range ports {
				if p.dial(net.JoinHostPort(host, strconv.Itoa(port))) == nil {
					mu.Lock()
					found = append(found, port)
					mu.Unlock()
				}
			}
		}()
	}
	for port := 1; port <= 65535; port++ {
		ports <- port
	}
	close(ports)
	wg.Wait()
	sort.Ints(found)

	var extra, missing []string
	got := map[int]bool{}
	for _, port := range found {
		got[port] = true
		if !want[port] {
			extra = append(extra, strconv.Itoa(port))
		}
	}
	for port := range want {
		if !got[port] {
			missing = append(missing, strconv.Itoa(port))
		}
	}
	sort.Strings(missing)

	detail := "open: " + joinInts(found)
	if len(extra) > 0 {
		detail += "; not expected: " + strings.Join(extra, ",")
	}
	if len(missing) > 0 {
		detail += "; expected but closed: " + strings.Join(missing, ",")
	}
	p.report(len(extra) == 0 && len(missing) == 0, "scan", host+" (1-65535)", detail)
}

func joinInts(ns []int) string {
	if len(ns) == 0 {
		return "none"
	}
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func serveForever(addr string, out io.Writer) int {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(out, "cannot listen:", err)
		return 1
	}
	fmt.Fprintln(out, "listening on", l.Addr())
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return 0
			}
			continue
		}
		c.Close()
	}
}
