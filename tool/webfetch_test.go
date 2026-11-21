package tool

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/redirect/")); err == nil {
			if count == 0 {
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "arrived")
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/redirect/%d", count-1), http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><style>a>b{color:red}</style><script>if (a < b) { alert('hide'); }</SCRIPT><!-- hidden --><p>One &amp; <b>two</b></p><p title="a > b">three</p></html>`)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "one\n  two")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, " {\"one\":2}\n")
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
		case "/redirect":
			http.Redirect(w, r, "/redirect", http.StatusFound)
		case "/redirect-public":
			http.Redirect(w, r, "http://127.0.0.2/hidden", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "bytes")
		}
	}))
	defer srv.Close()
	allowedHost, _, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fetch := WebFetch{dialCheck: func(ip netip.Addr) error {
		if ip.String() == allowedHost {
			return nil
		}
		return checkPublicIP(ip)
	}}
	for _, tc := range []struct{ path, text, errorText string }{
		{"/html", "One & two three", ""}, {"/text", "one\n  two", ""}, {"/json", " {\"one\":2}\n", ""},
		{"/large", "", "1 MiB"}, {"/redirect", "", "redirect"}, {"/redirect-public", "", "non-public"},
		{"/binary", "", "content type"}, {"/redirect/5", "arrived", ""}, {"/redirect/6", "", "exceeds 5 redirects"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"url": srv.URL + tc.path})
			got, err := fetch.Run(t.Context(), input)
			if tc.errorText == "" && (err != nil || got != tc.text) ||
				tc.errorText != "" && (err == nil || !strings.Contains(err.Error(), tc.errorText)) {
				t.Fatalf("fetch = %q, %v", got, err)
			}
		})
	}
	input, _ := json.Marshal(map[string]string{"url": srv.URL + "/html"})
	if _, err := (WebFetch{}).Run(t.Context(), input); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("loopback fetch = %v", err)
	}
}

func TestWebFetchAddressPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "100.64.0.1", "100.127.255.255", "::1", "::", "fd12::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "0.1.2.3", "198.19.255.255", "255.0.1.2", "192.0.0.8", "64:ff9b::7f00:1", "64:ff9b:1::1", "2002::1", "2001::1"} {
		if err := checkPublicIP(netip.MustParseAddr(address)); err == nil {
			t.Errorf("allowed %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "2606:4700::1111", "100.128.0.1"} {
		if err := checkPublicIP(netip.MustParseAddr(address)); err != nil {
			t.Errorf("refused %s: %v", address, err)
		}
	}
}
