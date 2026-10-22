package telemetry

import "testing"

func TestSplitOTLPEndpoint(t *testing.T) {
	cases := []struct {
		name               string
		endpoint           string
		insecureCfg        bool
		wantHost, wantPath string
		wantInsecure       bool
	}{
		{"http base appends default path", "http://c:4318", false, "c:4318", "", true},
		{"https base is secure", "https://c:4318", false, "c:4318", "", false},
		{"root path treated as empty", "http://c:4318/", false, "c:4318", "", true},
		{"explicit path preserved", "http://c:4318/v1/traces", false, "c:4318", "/v1/traces", true},
		{"https explicit path", "https://c/otlp", false, "c", "/otlp", false},
		{"insecure override on https", "https://c:4318", true, "c:4318", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p, ins := splitOTLPEndpoint(tc.endpoint, tc.insecureCfg)
			if h != tc.wantHost || p != tc.wantPath || ins != tc.wantInsecure {
				t.Fatalf("splitOTLPEndpoint(%q, %v) = (%q, %q, %v), want (%q, %q, %v)",
					tc.endpoint, tc.insecureCfg, h, p, ins, tc.wantHost, tc.wantPath, tc.wantInsecure)
			}
		})
	}
}
