package scenario

import (
	"net/url"
	"testing"
)

func TestTamperSignature(t *testing.T) {
	src := "/api/images/abc?exp=1900000000&sig=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tampered, expired := tamperSignature(src)
	tu, _ := url.Parse(tampered)
	if tu.Query().Get("sig") == "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || tu.Query().Get("exp") != "1900000000" {
		t.Fatalf("tampered = %s", tampered)
	}
	eu, _ := url.Parse(expired)
	if eu.Query().Get("exp") != "1700000000" || eu.Query().Get("sig") != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("expired = %s", expired)
	}
}

func TestCountryForRegion(t *testing.T) {
	for region, codes := range map[string][]string{"asia": {"JP", "KR", "CN", "AU"}, "europe": {"GB", "DE", "FR"}, "america": {"US", "CA", "MX"}} {
		got := countryFor(region, "some-user-id")
		ok := false
		for _, c := range codes {
			ok = ok || c == got
		}
		if !ok {
			t.Fatalf("%s → %s", region, got)
		}
	}
}
