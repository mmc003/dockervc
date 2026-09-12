package dockerapi

import (
	"testing"

	"github.com/docker/docker/api/types/network"
)

// Fixture matching the live-store shape of a captured network inspect
// (bridge driver, default IPAM, one subnet) plus the engine-owned fields the
// mapper must never forward.
const netInspectJSON = `{
  "Name": "demo-net",
  "Id": "7efba7060ca4dee87434c01f148785bc9c770a105502c042a8aa325c0e2df1aa",
  "Created": "2026-09-11T08:07:42.332022797Z",
  "Scope": "local",
  "Driver": "bridge",
  "EnableIPv4": true,
  "EnableIPv6": false,
  "IPAM": {
    "Driver": "default",
    "Options": {"opt": "val"},
    "Config": [{"Subnet": "172.18.0.0/16", "IPRange": "172.18.0.0/24", "Gateway": "172.18.0.1", "AuxiliaryAddresses": {"a": "172.18.0.5"}}]
  },
  "Internal": false,
  "Attachable": false,
  "ConfigFrom": {"Network": ""},
  "ConfigOnly": false,
  "Options": {"com.docker.network.enable_ipv4": "true"},
  "Labels": {"team": "demo"},
  "Containers": {"deadbeef": {"Name": "demo-web", "EndpointID": "x", "MacAddress": "y", "IPv4Address": "z", "IPv6Address": ""}}
}`

func TestNetworkCreateOptions(t *testing.T) {
	name, opts, err := networkCreateOptions([]byte(netInspectJSON))
	if err != nil {
		t.Fatalf("networkCreateOptions: %v", err)
	}
	if name != "demo-net" {
		t.Fatalf("name = %q, want demo-net", name)
	}
	if opts.Driver != "bridge" {
		t.Fatalf("Driver = %q, want bridge", opts.Driver)
	}
	if opts.Options["com.docker.network.enable_ipv4"] != "true" || opts.Labels["team"] != "demo" {
		t.Fatalf("Options/Labels not carried: %v / %v", opts.Options, opts.Labels)
	}
	// Engine-owned fields have no place in CreateOptions at all (there is no
	// field to carry Id/Created/Scope/Containers) — what must hold is that
	// the mapped options don't sneak them in via ConfigFrom/Ingress either.
	if opts.ConfigFrom != nil || opts.ConfigOnly || opts.Ingress || opts.Scope != "" {
		t.Fatalf("engine-owned config leaked: %+v", opts)
	}
	if opts.EnableIPv4 == nil || !*opts.EnableIPv4 {
		t.Fatalf("EnableIPv4 = %v, want explicit true (stored true)", opts.EnableIPv4)
	}
	if opts.EnableIPv6 != nil {
		t.Fatalf("EnableIPv6 = %v, want nil (stored false → daemon default)", opts.EnableIPv6)
	}
	if opts.IPAM == nil {
		t.Fatal("IPAM not carried")
	}
	want := network.IPAMConfig{Subnet: "172.18.0.0/16", IPRange: "172.18.0.0/24",
		Gateway: "172.18.0.1", AuxAddress: map[string]string{"a": "172.18.0.5"}}
	got := opts.IPAM.Config[0]
	if opts.IPAM.Driver != "default" || len(opts.IPAM.Config) != 1 ||
		got.Subnet != want.Subnet || got.IPRange != want.IPRange || got.Gateway != want.Gateway ||
		got.AuxAddress["a"] != "172.18.0.5" {
		t.Fatalf("IPAM = %+v, want driver default + %+v", opts.IPAM, want)
	}
	if opts.IPAM.Options["opt"] != "val" {
		t.Fatalf("IPAM.Options not carried: %v", opts.IPAM.Options)
	}
}

// demo-web-shaped container inspect: named-volume bind, published port,
// restart policy, an endpoint with an alias and a static IP that must NOT
// come back.
const containerInspectJSON = `{
  "Id": "7fb54f896c1e0d9f8f0e9ab5c0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9",
  "Created": "2026-09-11T08:07:42Z",
  "Name": "/demo-web",
  "State": {"Running": true, "Status": "running"},
  "Config": {
    "Hostname": "7fb54f896c1e",
    "Domainname": "",
    "User": "nginx",
    "Env": ["KEY=value", "PATH=/usr/bin"],
    "Cmd": ["nginx", "-g", "daemon off;"],
    "Entrypoint": ["/docker-entrypoint.sh"],
    "Image": "nginx:alpine",
    "WorkingDir": "/usr/share/nginx/html",
    "Labels": {"com.docker.compose.service": "web"},
    "StopSignal": "SIGQUIT",
    "Tty": false,
    "OpenStdin": false,
    "ExposedPorts": {"80/tcp": {}}
  },
  "HostConfig": {
    "Binds": ["demo-data:/data"],
    "NetworkMode": "demo-net",
    "PortBindings": {"80/tcp": [{"HostIp": "", "HostPort": "8080"}]},
    "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
    "Privileged": false,
    "ReadonlyRootfs": false,
    "Memory": 268435456,
    "NanoCPUs": 500000000,
    "CapAdd": ["NET_ADMIN"],
    "CapDrop": ["CHOWN"]
  },
  "NetworkSettings": {
    "Networks": {
      "demo-net": {
        "IPAMConfig": {"IPv4Address": "172.18.0.2"},
        "Links": null,
        "Aliases": ["demo-web", "web"],
        "MacAddress": "02:42:ac:12:00:02",
        "DriverOpts": null,
        "NetworkID": "7efba7060ca4",
        "EndpointID": "9b8a",
        "IPAddress": "172.18.0.2",
        "DNSNames": ["demo-web"]
      }
    }
  }
}`

// A load response is a JSON stream of progress events; only the final
// "Loaded image" line names the image — and it must yield the daemon's own
// image ID (tag-less save) or the restored ref (tagged save).
func TestLoadedImageRef(t *testing.T) {
	untagged := `{"stream":"Loaded from session id: abc\n"}
{"progressDetail":{},"id":"abc","status":"Loading layers"}
{"stream":"Loaded image ID: sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616\n"}
`
	if got := loadedImageRef(untagged); got != "sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616" {
		t.Fatalf("loadedImageRef(untagged) = %q", got)
	}
	tagged := `{"stream":"Loaded image: busybox:latest\n"}`
	if got := loadedImageRef(tagged); got != "busybox:latest" {
		t.Fatalf("loadedImageRef(tagged) = %q", got)
	}
	if got := loadedImageRef(`{"stream":"nope\n"}`); got != "" {
		t.Fatalf("loadedImageRef(no image line) = %q, want empty", got)
	}
}

func TestContainerCreateConfig(t *testing.T) {
	cfg, hc, nc, name, err := containerCreateConfig([]byte(containerInspectJSON), "dockervc/restore/snap-x/demo-web")
	if err != nil {
		t.Fatalf("containerCreateConfig: %v", err)
	}
	if name != "demo-web" {
		t.Fatalf("name = %q, want demo-web", name)
	}
	if cfg.Hostname != "" {
		t.Fatalf("Hostname = %q, want dropped (was the old container ID)", cfg.Hostname)
	}
	if cfg.Image != "dockervc/restore/snap-x/demo-web" {
		t.Fatalf("Image = %q, want the restored image ref", cfg.Image)
	}
	if len(cfg.Env) != 2 || cfg.Env[0] != "KEY=value" || cfg.Cmd[0] != "nginx" ||
		cfg.WorkingDir != "/usr/share/nginx/html" || cfg.Labels["com.docker.compose.service"] != "web" ||
		cfg.StopSignal != "SIGQUIT" || len(cfg.Entrypoint) != 1 || cfg.User != "nginx" {
		t.Fatalf("Config fields not carried verbatim: %+v", cfg)
	}
	if _, ok := cfg.ExposedPorts["80/tcp"]; !ok {
		t.Fatalf("ExposedPorts not carried: %v", cfg.ExposedPorts)
	}
	if len(hc.Binds) != 1 || hc.Binds[0] != "demo-data:/data" {
		t.Fatalf("Binds not carried: %v", hc.Binds)
	}
	if hc.NetworkMode != "demo-net" {
		t.Fatalf("NetworkMode = %q, want demo-net", hc.NetworkMode)
	}
	if hc.PortBindings["80/tcp"][0].HostPort != "8080" {
		t.Fatalf("PortBindings not carried: %v", hc.PortBindings)
	}
	if hc.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("RestartPolicy not carried: %+v", hc.RestartPolicy)
	}
	if hc.CapAdd[0] != "NET_ADMIN" || hc.CapDrop[0] != "CHOWN" {
		t.Fatalf("CapAdd/CapDrop not carried: %v / %v", hc.CapAdd, hc.CapDrop)
	}
	// Resources wholesale: one spot-check per family is enough.
	if hc.Memory != 268435456 || hc.NanoCPUs != 500000000 {
		t.Fatalf("Resources not carried wholesale: memory=%d nanoCPUs=%d", hc.Memory, hc.NanoCPUs)
	}
	ep := nc.EndpointsConfig["demo-net"]
	if ep == nil {
		t.Fatalf("EndpointsConfig missing demo-net: %+v", nc.EndpointsConfig)
	}
	if len(ep.Aliases) != 2 || ep.Aliases[0] != "demo-web" {
		t.Fatalf("Aliases not carried: %v", ep.Aliases)
	}
	if ep.IPAMConfig != nil {
		t.Fatalf("IPAMConfig = %+v, want nil (engine reassigns)", ep.IPAMConfig)
	}
	if ep.NetworkID != "" || ep.IPAddress != "" || ep.MacAddress != "" || len(ep.DNSNames) != 0 {
		t.Fatalf("operational endpoint fields leaked: %+v", ep)
	}
}
