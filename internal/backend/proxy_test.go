package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The hive's own addresses never go through a proxy: an agent link to
// 192.168.123.x sent to a proxy abroad would just fail.
func TestTheHivesOwnAddressesStayDirect(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true, "192.168.123.216": true, "10.1.2.3": true,
		"172.20.0.5": true, "100.101.102.103": true, "169.254.1.1": true, "orange1": true, "infra": true,
		"mac.local": true, "box.tail1234.ts.net": true, "": true,
		"api.openai.com": false, "8.8.8.8": false, "cpa.superleo.app": false, "100.128.0.1": false,
	} {
		if got := StaysDirect(host); got != want {
			t.Errorf("%q: direct %v, want %v", host, got, want)
		}
	}
}

const scutilClash = `<dictionary> {
  ExceptionsList : <array> {
    0 : 127.0.0.1
    1 : *.corp.example
    2 : 198.18.0.0/15
  }
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 7897
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 7897
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 1
  SOCKSPort : 7898
  SOCKSProxy : 127.0.0.1
}`

// Clash Verge's "system proxy" as macOS reports it.
func TestTheSystemProxyIsReadFromScutil(t *testing.T) {
	s := parseScutil(scutilClash)
	if s.https == nil || s.https.String() != "http://127.0.0.1:7897" || s.socks.String() != "socks5://127.0.0.1:7898" {
		t.Fatalf("%+v", s)
	}
	if got := s.forScheme("https"); got.String() != "http://127.0.0.1:7897" {
		t.Fatal(got)
	}
	if !s.excepted("git.corp.example") || !s.excepted("198.19.1.1") || s.excepted("api.openai.com") {
		t.Fatalf("exceptions %v", s.except)
	}
	off := parseScutil("<dictionary> {\n  HTTPEnable : 0\n  HTTPSEnable : 0\n  SOCKSEnable : 0\n}")
	if off.forScheme("https") != nil || off.String() != "none" {
		t.Fatalf("%+v", off)
	}
}

func TestEachModeTakesItsWay(t *testing.T) {
	out := &url.URL{Scheme: "https", Host: "api.openai.com"}
	lan := &url.URL{Scheme: "http", Host: "192.168.123.216:31744"}
	manual := ProxySettings{Mode: ProxyModeManual, URL: "127.0.0.1:7897"}
	if u, err := proxyForURL(manual, out); err != nil || u.String() != "http://127.0.0.1:7897" {
		t.Fatalf("%v %v", u, err)
	}
	if u, _ := proxyForURL(manual, lan); u != nil {
		t.Fatal("the hive went through the proxy")
	}
	if u, _ := proxyForURL(ProxySettings{Mode: ProxyModeOff}, out); u != nil {
		t.Fatal("off went through a proxy")
	}
	// Empty mode follows the environment: http.ProxyFromEnvironment caches
	// its answer for the process's life (sync.Once), so asserting "no proxy"
	// here would depend on no earlier test in this binary, nor this machine's
	// real HTTPS_PROXY/https_proxy, having touched it first. Checking that
	// proxyForURL's answer agrees with calling it directly holds regardless.
	urlStr := func(u *url.URL) string {
		if u == nil {
			return ""
		}
		return u.String()
	}
	want, _ := http.ProxyFromEnvironment(&http.Request{URL: out})
	if got, _ := proxyForURL(ProxySettings{}, out); urlStr(got) != urlStr(want) {
		t.Fatalf("empty mode should follow the environment: got %v, want %v", got, want)
	}
	for _, bad := range []ProxySettings{{Mode: ProxyModeManual}, {Mode: ProxyModeManual, URL: "ftp://x:1"}, {Mode: ProxyModeManual, URL: "http://nohost"}, {Mode: "clash"}} {
		if bad.Validate() == nil {
			t.Errorf("%+v passed", bad)
		}
	}
	if (ProxySettings{Mode: ProxyModeManual, URL: "socks5://127.0.0.1:7898"}).Validate() != nil {
		t.Error("socks5 refused")
	}
}

// The CLIs SuperLeo starts are given the same way out, and the hive's own
// addresses as NO_PROXY; with the setting off they are given none.
func TestStartedProgramsTakeTheSameWayOut(t *testing.T) {
	defer ApplyProxy(currentProxy())
	ApplyProxy(ProxySettings{})
	if ProxyEnv() != nil {
		t.Fatal("the environment mode changed the environment")
	}
	ApplyProxy(ProxySettings{Mode: ProxyModeManual, URL: "http://127.0.0.1:7897"})
	env := ProxyEnv()
	if env["HTTPS_PROXY"] != "http://127.0.0.1:7897" || !strings.Contains(env["NO_PROXY"], "192.168.0.0/16") {
		t.Fatalf("%v", env)
	}
	ApplyProxy(ProxySettings{Mode: ProxyModeOff})
	if env := ProxyEnv(); env["HTTPS_PROXY"] != "" || env["ALL_PROXY"] != "" {
		t.Fatalf("off left a proxy: %v", env)
	}
}

// A CLI that may outlive SuperLeo is never handed a LAN proxy when this
// machine runs one on loopback: once the app quits, macOS bars the orphan
// from the LAN. A loopback proxy set is kept, and no proxy stays no proxy.
func TestADetachedCLIGoesOutThroughLoopback(t *testing.T) {
	defer ApplyProxy(currentProxy())
	listening := map[string]bool{}
	old := localProxyListening
	localProxyListening = func(hp string) bool { return listening[hp] }
	defer func() { localProxyListening = old }()
	sysCache.mu.Lock()
	sysCache.val, sysCache.at = sysProxy{}, time.Now().Add(time.Hour)
	sysCache.mu.Unlock()
	defer systemCacheReset()

	lan := ProxySettings{Mode: ProxyModeManual, URL: "http://192.168.123.61:43127"}
	ApplyProxy(lan)
	if env := DetachedProxyEnv(); env["HTTPS_PROXY"] != "http://192.168.123.61:43127" {
		t.Fatalf("nothing local answers, so the LAN proxy should stay: %v", env)
	}
	listening["127.0.0.1:7890"] = true
	env := DetachedProxyEnv()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "ALL_PROXY", "all_proxy"} {
		if env[k] != "http://127.0.0.1:7890" {
			t.Fatalf("%s = %q, want the local Clash", k, env[k])
		}
	}
	if !strings.Contains(env["NO_PROXY"], "127.0.0.1") {
		t.Fatalf("SuperLeo's own servers must stay direct: %v", env)
	}
	if ProxyEnv()["HTTPS_PROXY"] != "http://192.168.123.61:43127" {
		t.Fatal("programs SuperLeo keeps an eye on should still take the proxy set")
	}

	// A system proxy on loopback wins over guessing ports.
	listening["127.0.0.1:7897"] = true
	sysCache.mu.Lock()
	sysCache.val = parseScutil(scutilClash)
	sysCache.mu.Unlock()
	if got := DetachedProxyEnv()["HTTPS_PROXY"]; got != "http://127.0.0.1:7897" {
		t.Fatalf("got %q, want the system proxy", got)
	}

	ApplyProxy(ProxySettings{Mode: ProxyModeManual, URL: "http://127.0.0.1:1087"})
	if got := DetachedProxyEnv()["HTTPS_PROXY"]; got != "http://127.0.0.1:1087" {
		t.Fatalf("a loopback proxy set should be kept, got %q", got)
	}
	ApplyProxy(ProxySettings{Mode: ProxyModeOff})
	if env := DetachedProxyEnv(); env["HTTPS_PROXY"] != "" || env["ALL_PROXY"] != "" {
		t.Fatalf("no proxy set should stay direct: %v", env)
	}
}

// A setting changed while running takes the next request, through Go's
// default transport, and the test of a URL not yet saved leaves the one in
// use alone.
func TestAProxyChangedWhileRunningTakesTheNextRequest(t *testing.T) {
	var through atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		through.Add(1)
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer px.Close()
	InstallProxy()
	defer ApplyProxy(currentProxy())
	// A public-looking name, so it is not one the hive keeps direct; only
	// the proxy answers it.
	target := "http://example.invalid/"
	ApplyProxy(ProxySettings{Mode: ProxyModeManual, URL: px.URL})
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if through.Load() != 1 {
		t.Fatalf("%d through the proxy", through.Load())
	}
	ApplyProxy(ProxySettings{Mode: ProxyModeOff})
	if _, err := http.Get(target); err == nil {
		t.Fatal("off still reached the proxy")
	}
	if res, err := TestProxy(context.Background(), ProxySettings{Mode: ProxyModeManual, URL: px.URL}, target); err != nil || !strings.HasPrefix(res, "HTTP 200") {
		t.Fatalf("%q %v", res, err)
	}
	if currentProxy().Mode != ProxyModeOff || through.Load() != 2 {
		t.Fatalf("the test touched the setting in use: %+v, %d", currentProxy(), through.Load())
	}
}
