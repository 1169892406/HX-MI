package main

import "testing"

func TestNormalizeTrustedSTS(t *testing.T) {
	ok := []string{
		"https://sts.api.io.mi.com/sts?d=abc&t=xyz",
		"  https://sts.api.io.mi.com/sts?d=abc  ",
		"\"https://account.xiaomi.com/pass/serviceLogin?callback=x\"",
		"HTTPS://STS.API.IO.MI.COM/sts?d=abc",
		"https://sts.api.io.mi.com./sts?d=abc",
		"https://cn.account.xiaomi.com/pass/x",
	}
	for _, raw := range ok {
		if loc, why := normalizeTrustedSTS(raw); loc == "" {
			t.Fatalf("%q 应可信，却被拒: %s", raw, why)
		}
	}
	bad := []string{
		"", "   ", "http://sts.api.io.mi.com/sts?d=1",
		"https://evil.example.com/sts", "https://xiaomi.com.evil.io/x",
		"not a link",
	}
	for _, raw := range bad {
		if loc, why := normalizeTrustedSTS(raw); loc != "" || why == "" {
			t.Fatalf("%q 应被拒且给出原因，得到 loc=%q why=%q", raw, loc, why)
		}
	}
}
