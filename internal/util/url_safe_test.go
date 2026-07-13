package util

import "testing"

func TestValidateCallbackURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"", false},
		{"https://merchant.example.com/notify", false},
		{"http://pay.example.com/callback?x=1", false},
		{"ftp://example.com/x", true},
		{"http://127.0.0.1/notify", true},
		{"http://localhost/notify", true},
		{"http://10.0.0.1/n", true},
		{"http://192.168.1.1/n", true},
		{"http://169.254.169.254/latest/meta-data", true},
		{"http://[::1]/n", true},
		{"javascript:alert(1)", true},
		{"http://0.0.0.0/", true},
		{"http://100.64.0.1/n", true},
		{"https://api.my-shop.com/pay/notify", false},
	}

	for _, tc := range cases {
		err := ValidateCallbackURL(tc.url)
		if tc.wantErr && err == nil {
			t.Errorf("ValidateCallbackURL(%q) expected error, got nil", tc.url)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ValidateCallbackURL(%q) unexpected error: %v", tc.url, err)
		}
	}
}

func TestIsPrivateOrReservedIP(t *testing.T) {
	// 通过 ValidateCallbackURL 间接覆盖主要 IP 类
	privates := []string{
		"http://127.0.0.1/",
		"http://10.1.2.3/",
		"http://172.16.0.1/",
		"http://192.168.0.1/",
		"http://169.254.1.1/",
		"http://100.64.10.1/",
	}
	for _, u := range privates {
		if err := ValidateCallbackURL(u); err == nil {
			t.Errorf("expected private block for %s", u)
		}
	}
}
