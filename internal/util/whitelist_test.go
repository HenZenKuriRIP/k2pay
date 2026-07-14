package util

import "testing"

func TestNormalizeIPWhitelist(t *testing.T) {
	got, err := NormalizeIPWhitelist(" 1.2.3.4 , 10.0.0.0/24\n1.2.3.4 ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3.4,10.0.0.0/24" {
		t.Fatalf("got %q", got)
	}
	_, err = NormalizeIPWhitelist("not-an-ip")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeDomainWhitelist(t *testing.T) {
	got, err := NormalizeDomainWhitelist("https://Shop.Example.com/path, *.cdn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "shop.example.com,*.cdn.example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestParseHostInput(t *testing.T) {
	h, err := ParseHostInput("https://pay.example.com:8443/api")
	if err != nil {
		t.Fatal(err)
	}
	if h != "pay.example.com" {
		t.Fatalf("got %q", h)
	}
}
