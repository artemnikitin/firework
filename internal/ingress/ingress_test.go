package ingress

import "testing"

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "simple", in: "example.com", want: "example.com"},
		{name: "multi-label", in: "gcp.example.com", want: "gcp.example.com"},
		{name: "trailing dot", in: "example.com.", want: "example.com"},
		{name: "surrounding whitespace", in: "  example.com  ", want: "example.com"},
		{name: "uppercase normalized", in: "Example.COM", want: "example.com"},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "scheme", in: "https://example.com", wantErr: true},
		{name: "port", in: "example.com:443", wantErr: true},
		{name: "path", in: "example.com/foo", wantErr: true},
		{name: "wildcard", in: "*.example.com", wantErr: true},
		{name: "empty label", in: "example..com", wantErr: true},
		{name: "leading hyphen", in: "-example.com", wantErr: true},
		{name: "trailing hyphen label", in: "example-.com", wantErr: true},
		{name: "overlong label", in: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeDomain(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeDomain(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("NormalizeDomain(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateSubdomain(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "tenant-1"},
		{name: "alnum", in: "myservice"},
		{name: "single char", in: "a"},
		{name: "dotted", in: "myservice.mysubdomain", wantErr: true},
		{name: "uppercase", in: "Tenant-1", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "leading hyphen", in: "-tenant", wantErr: true},
		{name: "trailing hyphen", in: "tenant-", wantErr: true},
		{name: "overlong", in: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: true},
		{name: "wildcard", in: "*", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSubdomain(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSubdomain(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateHost(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "fqdn", in: "custom.example.net", want: "custom.example.net"},
		{name: "internal single label", in: "internal-host", want: "internal-host"},
		{name: "trailing dot", in: "custom.example.net.", want: "custom.example.net"},
		{name: "uppercase normalized", in: "Custom.Example.NET", want: "custom.example.net"},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "  ", wantErr: true},
		{name: "backtick injection", in: "a`b.example.com", wantErr: true},
		{name: "rule injection", in: "x.com`) || Host(`y.com", wantErr: true},
		{name: "scheme", in: "http://custom.example.net", wantErr: true},
		{name: "port", in: "custom.example.net:8080", wantErr: true},
		{name: "wildcard", in: "*.example.net", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateHost(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateHost(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("ValidateHost(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		meta    map[string]string
		domain  string
		want    string
		wantErr bool
	}{
		{name: "no keys", meta: map[string]string{}, domain: "example.com", want: ""},
		{name: "nil metadata", meta: nil, domain: "example.com", want: ""},
		{name: "subdomain + domain", meta: map[string]string{"subdomain": "tenant-1"}, domain: "gcp.example.com", want: "tenant-1.gcp.example.com"},
		{name: "subdomain plain domain", meta: map[string]string{"subdomain": "tenant-1"}, domain: "example.com", want: "tenant-1.example.com"},
		{name: "exact host ignores domain", meta: map[string]string{"host": "custom.example.net"}, domain: "example.com", want: "custom.example.net"},
		{name: "exact host empty domain", meta: map[string]string{"host": "custom.example.net"}, domain: "", want: "custom.example.net"},
		{name: "subdomain missing domain", meta: map[string]string{"subdomain": "tenant-1"}, domain: "", wantErr: true},
		{name: "subdomain whitespace domain", meta: map[string]string{"subdomain": "tenant-1"}, domain: "  ", wantErr: true},
		{name: "both keys", meta: map[string]string{"subdomain": "tenant-1", "host": "custom.example.net"}, domain: "example.com", wantErr: true},
		{name: "empty subdomain value", meta: map[string]string{"subdomain": ""}, domain: "example.com", wantErr: true},
		{name: "whitespace subdomain value", meta: map[string]string{"subdomain": "  "}, domain: "example.com", wantErr: true},
		{name: "empty host value", meta: map[string]string{"host": ""}, domain: "example.com", wantErr: true},
		{name: "dotted subdomain", meta: map[string]string{"subdomain": "a.b"}, domain: "example.com", wantErr: true},
		{name: "invalid host", meta: map[string]string{"host": "bad host"}, domain: "example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve("svc", tt.meta, tt.domain)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve(%v,%q) err=%v, wantErr=%v", tt.meta, tt.domain, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("Resolve(%v,%q)=%q, want %q", tt.meta, tt.domain, got, tt.want)
			}
		})
	}
}
