package main

import "testing"

func TestAdminMustListenOnLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8099", "127.0.0.2:1", "[::1]:8099"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8099", ":8099", "[::]:8099", "localhost:8099", "172.18.0.1:8099", "gateway:8099", "127.0.0.1"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
