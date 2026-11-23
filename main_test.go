package main

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testSecret(t *testing.T) []byte {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	return secret
}

func TestCookieNameFor(t *testing.T) {
	// 与内核 cookieName 的算法一致：dsh-auth- + base64url(sha256(authority))。
	got := cookieNameFor("127.0.0.1:3080")
	want := "dsh-auth-VPhEEcLKeqRDBoBalzN2Nm7CnfxKhLE00pKIDWxt1sw"
	if got != want {
		t.Fatalf("cookieNameFor = %q, want %q", got, want)
	}
}

func TestMintCookieRoundTrip(t *testing.T) {
	secret := testSecret(t)
	authority := "127.0.0.1:3080"

	value := mintCookie(secret, authority, time.Hour)
	if !verifyCoreCookie(value, secret, authority) {
		t.Fatal("代签的 Cookie 未通过校验")
	}
	if verifyCoreCookie(value, secret, "127.0.0.1:3081") {
		t.Fatal("authority 不匹配的 Cookie 不应通过")
	}
}

func TestVerifyCoreCookieRejects(t *testing.T) {
	secret := testSecret(t)
	authority := "127.0.0.1:3080"
	valid := mintCookie(secret, authority, time.Hour)
	// 改动签名首字符，保持 base64url 合法，只破坏 HMAC。
	at := strings.LastIndex(valid, ".") + 1
	replacement := "A"
	if valid[at] == 'A' {
		replacement = "B"
	}
	tampered := valid[:at] + replacement + valid[at+1:]

	cases := []struct {
		name  string
		value string
		key   []byte
	}{
		{"空值", "", secret},
		{"版本不对", "v2.abc.def", secret},
		{"签名被篡改", tampered, secret},
		{"已过期", mintCookie(secret, authority, -time.Hour), secret},
		{"没有密钥", mintCookie(nil, authority, time.Hour), nil},
	}
	for _, tc := range cases {
		if verifyCoreCookie(tc.value, tc.key, authority) {
			t.Errorf("%s: 不应通过校验", tc.name)
		}
	}
}

func TestRewriteHeadersMintDisabled(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.test/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "dsh-auth-other", Value: "keep"})

	s := &sidecar{authority: "127.0.0.1:3080", cookieName: cookieNameFor("127.0.0.1:3080")}
	s.rewriteHeaders(req)

	if got := req.Header.Get("Cookie"); got != "dsh-auth-other=keep" {
		t.Fatalf("禁用代签后 Cookie 应原样透传，得到 %q", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
}

func TestRewriteHeadersMintEnabled(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "dsh-auth-other", Value: "keep"})
	req.AddCookie(&http.Cookie{Name: "dsh-auth-stale", Value: "drop"})

	s := &sidecar{
		authority:   "127.0.0.1:3080",
		cookieName:  "dsh-auth-stale",
		cookieValue: "fresh",
		mint:        true,
	}
	s.rewriteHeaders(req)

	seen := map[string]string{}
	for _, c := range req.Cookies() {
		seen[c.Name] = c.Value
	}
	if seen["dsh-auth-other"] != "keep" {
		t.Fatalf("其它 Cookie 应保留，得到 %v", seen)
	}
	if seen["dsh-auth-stale"] != "fresh" {
		t.Fatalf("同名 Cookie 应被替换为代签值，得到 %v", seen)
	}
}

func TestMaybeClearSessionCookie(t *testing.T) {
	logout := func() *http.Response {
		r, err := http.NewRequest(http.MethodPost, "http://example.test/auth/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Request: r, Header: http.Header{}}
	}

	minting := &sidecar{mint: true, cookieName: "dsh-auth-x"}
	resp := logout()
	minting.maybeClearSessionCookie(resp)
	if !strings.Contains(resp.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal("代签模式下登出应下发清除 Cookie")
	}

	disabled := &sidecar{mint: false, cookieName: "dsh-auth-x"}
	resp = logout()
	disabled.maybeClearSessionCookie(resp)
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("禁用代签后不应改写浏览器 Cookie")
	}

	failed := logout()
	failed.StatusCode = http.StatusBadRequest
	minting.maybeClearSessionCookie(failed)
	if failed.Header.Get("Set-Cookie") != "" {
		t.Fatal("登出失败时不应清除 Cookie")
	}
}
