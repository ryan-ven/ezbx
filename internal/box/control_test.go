package box

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseHosts(t *testing.T) {
	got, err := ParseHosts([]byte("# comment\n\nRegistry.npmjs.org\n  github.com  \n*.example.com\ngithub.com\n"), "f")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "*.example.com,github.com,registry.npmjs.org" {
		t.Error(got)
	}
	for _, bad := range []string{"192.0.2.1", "*.com", "example.com:443", "http://example.com", "exa mple.com", "*"} {
		if _, err := ParseHosts([]byte("github.com\n"+bad+"\n"), "f"); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestApprovedFile(t *testing.T) {
	f := ApprovedFileFor(t.TempDir())
	if hosts, err := ReadHosts(f); err != nil || hosts != nil {
		t.Fatal("a missing file is an empty list:", hosts, err)
	}
	if err := EnsureHosts(f); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", st.Mode().Perm())
		}
	}
	if added, err := AddHost(f, "GitHub.com"); err != nil || !added {
		t.Fatal(added, err)
	}
	if added, _ := AddHost(f, "github.com"); added {
		t.Error("a repeat is not new")
	}
	AddHost(f, "registry.npmjs.org")
	if _, err := AddHost(f, "10.0.0.1"); err == nil {
		t.Error("addresses are refused")
	}
	hosts, _ := ReadHosts(f)
	if strings.Join(hosts, ",") != "github.com,registry.npmjs.org" {
		t.Error(hosts)
	}
	// EnsureHosts keeps what is there.
	EnsureHosts(f)
	if again, _ := ReadHosts(f); len(again) != 2 {
		t.Error("EnsureHosts emptied the file")
	}
	if removed, _ := RemoveHost(f, "GITHUB.com"); !removed {
		t.Error("should remove")
	}
	if removed, _ := RemoveHost(f, "github.com"); removed {
		t.Error("already gone")
	}
	hosts, _ = ReadHosts(f)
	if strings.Join(hosts, ",") != "registry.npmjs.org" {
		t.Error(hosts)
	}
	if _, err := os.Stat(f + ".new"); err == nil {
		t.Error("temporary file left behind")
	}
}

func TestGatewayGetsApprovedHosts(t *testing.T) {
	p := testPlan(t)
	a := p.GatewayArgs()
	if !has(a, "-v", p.ApprovedFile()+":/run/ezbx/approved-hosts:ro") || !has(a, "--allow-file", "/run/ezbx/approved-hosts") {
		t.Errorf("gateway args: %v", a)
	}
	for _, x := range p.BoxArgs() {
		if strings.Contains(x, "approved-hosts") {
			t.Error("the box must not see the approved hosts file")
		}
	}
}

func TestUpMakesAndChecksApprovedFile(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	if err := runner(f).Up(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.ApprovedFile()); err != nil {
		t.Fatal("no approved-hosts file:", err)
	}

	f = newFake()
	p = testPlan(t)
	os.MkdirAll(p.StateDir, 0o700)
	os.WriteFile(p.ApprovedFile(), []byte("github.com\n10.0.0.1\n"), 0o600)
	err := runner(f).Up(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatal(err)
	}
	if f.did("create") {
		t.Error("no container should be made with a broken file")
	}
}

func TestRecentBlocked(t *testing.T) {
	log := strings.Join([]string{
		`{"time":"t1","route":"proxy","method":"CONNECT","host":"github.com:443","status":403,"blocked":true,"reason":"not_on_allowlist"}`,
		`{"time":"t2","route":"proxy","method":"CONNECT","host":"GitHub.com:443","status":403,"blocked":true,"reason":"not_on_allowlist"}`,
		`{"time":"t3","route":"proxy","method":"GET","host":"pypi.org","status":403,"blocked":true,"reason":"network_off"}`,
		`{"time":"t4","route":"proxy","method":"CONNECT","host":"localtest.me:443","status":403,"blocked":true,"reason":"local_address"}`,
		`{"time":"t5","route":"proxy","method":"CONNECT","host":"192.0.2.1:443","status":403,"blocked":true,"reason":"not_on_allowlist"}`,
		`{"time":"t6","route":"proxy","method":"CONNECT","host":"example.com:443","status":200}`,
		`{"time":"t7","route":"model","path":"/v1/models","status":200}`,
		`not json`,
	}, "\n")
	got := RecentBlocked([]byte(log))
	if len(got) != 2 || got[0].Host != "github.com" || got[0].Count != 2 || got[0].Last != "t2" || got[1].Host != "pypi.org" || got[1].Reason != "network_off" {
		t.Errorf("%+v", got)
	}
}

func TestTailFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "log")
	os.WriteFile(f, []byte("first line\nsecond\nthird\n"), 0o600)
	got, err := TailFile(f, 10)
	if err != nil || string(got) != "third\n" {
		t.Errorf("%q %v", got, err)
	}
	got, _ = TailFile(f, 1000)
	if string(got) != "first line\nsecond\nthird\n" {
		t.Errorf("%q", got)
	}
}

func TestAdminNeedsARunningGateway(t *testing.T) {
	f := newFake()
	r := runner(f)
	_, err := r.Admin(context.Background(), "t", "status")
	if _, ok := err.(ErrNotUp); !ok {
		t.Fatalf("got %v", err)
	}
	f.containers["ezbx-t-gateway"] = false
	if _, err := r.Admin(context.Background(), "t", "status"); err == nil || !strings.Contains(err.Error(), "didn't create it") {
		t.Fatalf("got %v", err)
	}
	if f.did("exec") {
		t.Error("exec into a container EZBox didn't make")
	}
}

func TestAdminCalls(t *testing.T) {
	f := newFake()
	f.containers["ezbx-t-gateway"] = true
	f.health = []string{"true"}
	r := runner(f)
	inner := f.run
	var exec [][]string
	r.Docker = func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if args[0] == "exec" {
			exec = append(exec, args)
			return []byte(`{"mode":"on","allow":["github.com"],"closed":2}`), nil
		}
		return inner(ctx, stdin, args...)
	}
	mode, allow, closed, err := r.SetNetwork(context.Background(), "t", "on", []string{"github.com"})
	if err != nil || mode != "on" || len(allow) != 1 || closed != 2 {
		t.Fatal(mode, allow, closed, err)
	}
	want := "exec ezbx-t-gateway /ezbx gateway admin --addr 127.0.0.1:8099 network on github.com"
	if len(exec) != 1 || strings.Join(exec[0], " ") != want {
		t.Errorf("got %v, want %s", exec, want)
	}
}

func TestCopyHome(t *testing.T) {
	f := newFake()
	p := testPlan(t)
	if err := runner(f).CopyHome(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !f.did("cp", "-a", "-", "ezbx-t-box:/home/agent") || len(f.stdin) == 0 {
		t.Error(f.calls)
	}
}
