package server

import "testing"

func TestValidateShortcutURL(t *testing.T) {
	ok := []string{
		"https://example.com",
		"http://100.64.0.1:8080/path?q=1",
		"https://ca-central-1.console.aws.amazon.com/ses/home?region=ca-central-1#/get-set-up",
		"https://grafana.example.com/d/abc#panel-2",
	}
	for _, u := range ok {
		if err := validateShortcutURL(u); err != nil {
			t.Errorf("%q should be accepted: %v", u, err)
		}
	}
	bad := []string{
		"javascript:alert(1)",
		"ftp://example.com",
		"/relative",
		"https://",
		"https://user:pw@example.com",
		"https://example.com:0",
		"https://example.com:99999",
	}
	for _, u := range bad {
		if err := validateShortcutURL(u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}
