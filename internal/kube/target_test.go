package kube

import "testing"

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      target
		wantError bool
	}{
		{
			name: "命名端口",
			raw:  "kube://production/game:connect",
			want: target{namespace: "production", service: "game", port: "connect"},
		},
		{
			name: "数字端口与TLS",
			raw:  "kube://production/game:8443?transport=https",
			want: target{namespace: "production", service: "game", port: "8443", portValue: 8443, transport: transportHTTPS},
		},
		{name: "错误scheme", raw: "kubernetes://production/game:connect", wantError: true},
		{name: "缺少namespace", raw: "kube:///game:connect", wantError: true},
		{name: "缺少端口", raw: "kube://production/game", wantError: true},
		{name: "非法查询参数", raw: "kube://production/game:connect?foo=bar", wantError: true},
		{name: "非法传输", raw: "kube://production/game:connect?transport=http1", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseTarget(test.raw)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseTarget(%q) 未返回错误", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("parseTarget(%q) = %#v, want %#v", test.raw, got, test.want)
			}
		})
	}
}
