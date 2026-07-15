package main

import "testing"

func TestBuildRegistry(t *testing.T) {
	cfg := Config{
		OIDCEnabled: true, OIDCIssuer: "https://i", OIDCClientID: "c", OIDCClientSecret: "s",
		VKEnabled: true, VKClientID: "vc", VKClientSecret: "vs",
	}
	reg := buildRegistry(cfg)
	list := reg.List()
	if len(list) != 2 || list[0].Name() != "oidc" || list[1].Name() != "vk" {
		t.Fatalf("registry list = %v", list)
	}
	if !buildRegistry(Config{}).Empty() {
		t.Fatal("no providers → empty registry")
	}
}
