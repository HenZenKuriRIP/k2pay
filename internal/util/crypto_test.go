package util

import "testing"

func TestIsWeakPassword(t *testing.T) {
	weak := []string{"admin123", "123456", "password", "merchant123", "abc", "12345678"}
	for _, p := range weak {
		if !IsWeakPassword(p) {
			t.Errorf("expected weak: %q", p)
		}
	}
	strong := []string{"MyS3cure!Pass", "Tr0ngPassw0rd", "helloWorld99"}
	for _, p := range strong {
		if IsWeakPassword(p) {
			t.Errorf("expected strong: %q", p)
		}
	}
}

func TestValidateNewPassword(t *testing.T) {
	if err := ValidateNewPassword("short"); err == nil {
		t.Error("expected error for short password")
	}
	if err := ValidateNewPassword("admin123"); err == nil {
		t.Error("expected error for weak password")
	}
	if err := ValidateNewPassword("GoodPass99x"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOrderPollToken(t *testing.T) {
	InitOrderPollSecret("test-secret-key")
	tradeNo := "20260101120000abcdef"
	token := GenerateOrderPollToken(tradeNo)
	if len(token) != 24 {
		t.Fatalf("token length = %d, want 24", len(token))
	}
	if !VerifyOrderPollToken(tradeNo, token) {
		t.Error("valid token should verify")
	}
	if VerifyOrderPollToken(tradeNo, "invalidtoken00000000000") {
		t.Error("invalid token should not verify")
	}
	if VerifyOrderPollToken(tradeNo, "") {
		t.Error("empty token should not verify")
	}
	// different secret => different token
	InitOrderPollSecret("other-secret")
	if VerifyOrderPollToken(tradeNo, token) {
		t.Error("token from other secret should not verify")
	}
}
