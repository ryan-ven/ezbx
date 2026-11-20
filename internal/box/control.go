package box

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ErrNotUp is returned when a box's containers aren't running.
type ErrNotUp struct{ Box, What string }

func (e ErrNotUp) Error() string {
	return fmt.Sprintf("box %q is not up (%s). Start it with: ezbx up", e.Box, e.What)
}

// Running reports whether a container exists and is running. It refuses a
// container with that name that EZBox didn't create.
func (r *Runner) Running(ctx context.Context, name string) (bool, error) {
	f, err := r.lookup(ctx, "container", name)
	if err != nil || !f.exists {
		return false, err
	}
	if !f.managed {
		return false, fmt.Errorf("a container named %s exists, but EZBox didn't create it, so EZBox won't use it", name)
	}
	out, err := r.run(ctx, "container", "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// Admin calls the gateway's admin API through docker exec, the same way the
// healthcheck and the doctor do. The token is read inside the gateway.
func (r *Runner) Admin(ctx context.Context, box string, call ...string) ([]byte, error) {
	gw := NamesFor(box).Gateway
	up, err := r.Running(ctx, gw)
	if err != nil {
		return nil, err
	}
	if !up {
		return nil, ErrNotUp{Box: box, What: "its gateway isn't running"}
	}
	args := append([]string{"exec", gw, "/ezbx", "gateway", "admin", "--addr", adminAddr}, call...)
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("the gateway refused %q: %w", strings.Join(call, " "), err)
	}
	return out, nil
}

// Status is what the gateway's admin API reports.
type Status struct {
	Engine   string `json:"engine"`
	Upstream string `json:"upstream"`
	Network  *struct {
		Mode  string   `json:"mode"`
		Allow []string `json:"allow"`
	} `json:"network"`
	Proxy *struct {
		Allowed int64 `json:"allowed"`
		Blocked int64 `json:"blocked"`
		Failed  int64 `json:"failed"`
		Open    int64 `json:"open"`
	} `json:"proxy"`
	UptimeS int64 `json:"uptime_s"`
}

// GatewayStatus reads the running gateway's state.
func (r *Runner) GatewayStatus(ctx context.Context, box string) (Status, error) {
	out, err := r.Admin(ctx, box, "status")
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(out, &s); err != nil {
		return Status{}, fmt.Errorf("unexpected answer from the gateway: %w", err)
	}
	if s.Network == nil {
		return Status{}, fmt.Errorf("the gateway of box %q runs without its egress proxy", box)
	}
	return s, nil
}

// SetNetwork sets the mode and the whole allowlist. It returns the new state
// and how many open connections the change closed.
func (r *Runner) SetNetwork(ctx context.Context, box, mode string, allow []string) (string, []string, int, error) {
	out, err := r.Admin(ctx, box, append([]string{"network", mode}, allow...)...)
	if err != nil {
		return "", nil, 0, err
	}
	var rep struct {
		Mode   string   `json:"mode"`
		Allow  []string `json:"allow"`
		Closed int      `json:"closed"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return "", nil, 0, fmt.Errorf("unexpected answer from the gateway: %w", err)
	}
	return rep.Mode, rep.Allow, rep.Closed, nil
}

// CopyHome copies rendered harness configs into the box's home, owned by the
// agent. It works on a running box.
func (r *Runner) CopyHome(ctx context.Context, p Plan) error {
	data, err := p.HomeTar()
	if err != nil {
		return err
	}
	if _, err := r.Docker(ctx, data, p.HomeCopyArgs()...); err != nil {
		return fmt.Errorf("copying the harness config into the box: %w", err)
	}
	return nil
}
