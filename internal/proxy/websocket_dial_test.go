package proxy

import (
	"net/url"
	"testing"
)

func TestWebSocketDialAddr(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare host ws", raw: "ws://emby.example.com/socket", want: "emby.example.com:80"},
		{name: "bare host wss", raw: "wss://emby.example.com/socket", want: "emby.example.com:443"},
		{name: "bare host http", raw: "http://emby.example.com/socket", want: "emby.example.com:80"},
		{name: "bare host https", raw: "https://emby.example.com/socket", want: "emby.example.com:443"},
		{name: "uppercase scheme", raw: "WSS://emby.example.com/socket", want: "emby.example.com:443"},
		{name: "host with port", raw: "ws://emby.example.com:8096/socket", want: "emby.example.com:8096"},
		{name: "host with port wss", raw: "wss://emby.example.com:8920/socket", want: "emby.example.com:8920"},
		{name: "bare ipv4", raw: "ws://127.0.0.1/socket", want: "127.0.0.1:80"},
		{name: "ipv4 with port", raw: "ws://127.0.0.1:8096/socket", want: "127.0.0.1:8096"},
		{name: "bare ipv6", raw: "ws://[::1]/socket", want: "[::1]:80"},
		{name: "bare ipv6 wss", raw: "wss://[::1]/socket", want: "[::1]:443"},
		{name: "ipv6 with port", raw: "ws://[::1]:8096/socket", want: "[::1]:8096"},
		{name: "full ipv6 with port", raw: "https://[2001:db8::1]:8920/socket", want: "[2001:db8::1]:8920"},
		{name: "bare full ipv6", raw: "https://[2001:db8::1]/socket", want: "[2001:db8::1]:443"},
		{name: "empty port falls back to default", raw: "ws://emby.example.com:/socket", want: "emby.example.com:80"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.raw, err)
			}
			if got := webSocketDialAddr(target); got != tc.want {
				t.Fatalf("webSocketDialAddr(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
