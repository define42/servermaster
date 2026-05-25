package main

import (
	"context"
	"testing"
)

func TestCollectServermasterStatus(t *testing.T) {
	links, addrs := testNetworkLinks()
	defer stubNetlink(links, addrs, nil, nil)()

	f := &fakePodman{
		list:    []listedContainer{{ID: "abc", Names: []string{"web"}, State: "running"}},
		inspect: map[string]containerInspectResponse{"abc": {ID: "abc", Name: "/web"}},
	}
	f.start(t)

	cfgPath := writeTempConfig(t, `{"podman_mode":"rootful"}`)
	t.Setenv("PATH", "") // ostree tooling unavailable -> recorded as an error

	status := collectServermasterStatus(context.Background(), cfgPath)
	if status.Status != "degraded" {
		t.Fatalf("status = %q, want degraded (ostree unavailable)", status.Status)
	}
	if len(status.Containers) != 1 || status.Containers[0].Name != "web" {
		t.Fatalf("containers = %+v", status.Containers)
	}
	if status.Network.Source != "netlink" {
		t.Fatalf("network source = %q, want netlink", status.Network.Source)
	}
}
