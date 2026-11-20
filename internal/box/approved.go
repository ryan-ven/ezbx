package box

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ryan-ven/ezbx/internal/config"
)

// Hosts approved with "ezbx net allow" are kept in the box's state folder,
// one per line, so they survive ezbx down and up and a restart of Docker.
// The network mode is not kept: every up starts in the mode the config says.

// ApprovedFile is where a box's approved hosts are kept on the host.
func (p Plan) ApprovedFile() string { return ApprovedFileFor(p.StateDir) }

// ApprovedFileFor is the approved-hosts file in a box's state folder.
func ApprovedFileFor(stateDir string) string { return filepath.Join(stateDir, "approved-hosts") }

const approvedHeader = "# Hosts approved with: ezbx net allow HOST\n# One per line. Remove one with: ezbx net remove HOST\n"

// ReadHosts reads a hosts file: one host per line, with blank lines and
// lines starting with # ignored. A missing file is an empty list. Every host
// is checked, so a broken file is an error rather than a wider allowlist.
func ReadHosts(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseHosts(data, file)
}

// ParseHosts reads the hosts file format from data. name is used in errors.
func ParseHosts(data []byte, name string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		h := strings.ToLower(strings.TrimSpace(sc.Text()))
		if h == "" || strings.HasPrefix(h, "#") {
			continue
		}
		if !config.HostName(h) {
			return nil, fmt.Errorf("%s line %d: %q is not a host name such as registry.npmjs.org or *.example.com", name, n, h)
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// writeHosts replaces the file's contents in one step, readable only by
// this user.
func writeHosts(file string, hosts []string) error {
	var b strings.Builder
	b.WriteString(approvedHeader)
	for _, h := range hosts {
		b.WriteString(h + "\n")
	}
	// The box may never have been up, so its state folder may not exist yet.
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// EnsureHosts creates an empty approved-hosts file if there is none, so it
// can be mounted into the gateway. (Docker would make a folder for a missing
// file.)
func EnsureHosts(file string) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeHosts(file, nil)
}

// AddHost saves one approved host. It reports whether it was new.
func AddHost(file, host string) (bool, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !config.HostName(host) {
		return false, fmt.Errorf("%q is not a host name such as registry.npmjs.org or *.example.com", host)
	}
	hosts, err := ReadHosts(file)
	if err != nil {
		return false, err
	}
	for _, h := range hosts {
		if h == host {
			return false, nil
		}
	}
	hosts = append(hosts, host)
	sort.Strings(hosts)
	return true, writeHosts(file, hosts)
}

// RemoveHost forgets one approved host. It reports whether it was saved.
func RemoveHost(file, host string) (bool, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	hosts, err := ReadHosts(file)
	if err != nil {
		return false, err
	}
	var keep []string
	for _, h := range hosts {
		if h != host {
			keep = append(keep, h)
		}
	}
	if len(keep) == len(hosts) {
		return false, nil
	}
	return true, writeHosts(file, keep)
}
