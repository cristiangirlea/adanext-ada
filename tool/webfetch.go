package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/cristiangirlea/adanext-ada/provider"
)

const webFetchLimit = 1 << 20

type WebFetch struct {
	dialCheck func(netip.Addr) error
}

func (WebFetch) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "fetch_url",
		Description: "Fetch public HTTP or HTTPS text or JSON, up to 1 MiB.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`),
	}
}

func (WebFetch) Effect() Effect { return Read }

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
}

func checkPublicIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
		return fmt.Errorf("refused non-public address %s", ip)
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return fmt.Errorf("refused non-public address %s", ip)
		}
	}
	return nil
}

func webURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

func (t WebFetch) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct{ URL string }
	if err := decodeInput(raw, &input); err != nil {
		return "", err
	}
	u, err := url.Parse(input.URL)
	if err != nil || !webURL(u) {
		return "", fmt.Errorf("fetch_url requires an HTTP or HTTPS URL")
	}
	check := t.dialCheck
	if check == nil {
		check = checkPublicIP
	}
	dialer := net.Dialer{
		// Control sees the actual address after resolution and runs again for redirect dials.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("inspect fetch address: %w", err)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("inspect fetch IP: %w", err)
			}
			return check(ip)
		},
	}
	transport := &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return fmt.Errorf("fetch_url exceeds 5 redirects")
			}
			if !webURL(req.URL) {
				return fmt.Errorf("fetch_url redirects require HTTP or HTTPS")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create fetch request: %w", err)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch_url: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("fetch_url returned HTTP %d", res.StatusCode)
	}
	kind, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || (!strings.HasPrefix(kind, "text/") && kind != "application/json") {
		return "", fmt.Errorf("fetch_url refused content type %q", res.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, webFetchLimit+1))
	if err != nil {
		return "", fmt.Errorf("read fetch response: %w", err)
	}
	if len(body) > webFetchLimit {
		return "", fmt.Errorf("fetch_url response exceeds 1 MiB")
	}
	if kind == "text/html" {
		return htmlText(string(body)), nil
	}
	return string(body), nil
}

func htmlText(page string) string {
	var out strings.Builder
	for len(page) > 0 {
		i := strings.IndexByte(page, '<')
		if i < 0 {
			out.WriteString(page)
			break
		}
		out.WriteString(page[:i])
		page = page[i:]
		if strings.HasPrefix(page, "<!--") {
			end := strings.Index(page[4:], "-->")
			if end < 0 {
				break
			}
			page = page[4+end+3:]
			continue
		}
		nameStart := 1
		closing := strings.HasPrefix(page, "</")
		if closing {
			nameStart++
		}
		if len(page) <= nameStart || !asciiLetter(page[nameStart]) && !strings.HasPrefix(page, "<!") && !strings.HasPrefix(page, "<?") {
			out.WriteByte('<')
			page = page[1:]
			continue
		}
		end := tagEnd(page)
		if end < 0 {
			break
		}
		nameEnd := nameStart
		for nameEnd < end && !strings.ContainsRune(" \t\n\r\f/>", rune(page[nameEnd])) {
			nameEnd++
		}
		name := strings.ToLower(page[nameStart:nameEnd])
		page = page[end+1:]
		switch name {
		case "script", "style":
			if !closing {
				page = skipRawElement(page, name)
			}
		case "address", "article", "aside", "blockquote", "br", "div", "dl", "dt", "dd", "footer", "form",
			"h1", "h2", "h3", "h4", "h5", "h6", "header", "hr", "li", "main", "nav", "ol", "p",
			"pre", "section", "table", "td", "th", "tr", "ul":
			out.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(html.UnescapeString(out.String())), " ")
}

func asciiLetter(b byte) bool {
	b |= 0x20
	return 'a' <= b && b <= 'z'
}

func tagEnd(tag string) int {
	var quote byte
	for i := 1; i < len(tag); i++ {
		b := tag[i]
		switch {
		case quote != 0:
			if b == quote {
				quote = 0
			}
		case b == '"' || b == '\'':
			quote = b
		case b == '>':
			return i
		}
	}
	return -1
}

func skipRawElement(page, name string) string {
	for {
		i := strings.Index(page, "</")
		if i < 0 {
			return ""
		}
		page = page[i:]
		end := 2 + len(name)
		if len(page) > end && strings.EqualFold(page[2:end], name) && strings.ContainsRune(" \t\n\r\f/>", rune(page[end])) {
			if close := tagEnd(page); close >= 0 {
				return page[close+1:]
			}
			return ""
		}
		page = page[2:]
	}
}
