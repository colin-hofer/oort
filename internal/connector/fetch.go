package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxPages     = 100
	maxPageBytes = 10 << 20
	maxTotal     = 100 << 20
)

type Config struct {
	URL               string
	BearerToken       string
	RecordsPointer    string
	CursorParameter   string
	NextCursorPointer string
	AllowPrivate      bool
}

type Result struct {
	Rows  int64
	Bytes int64
}

type HTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("connector returned HTTP %d", e.Status) }
func (e *HTTPError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func Fetch(ctx context.Context, config Config, output io.Writer) (Result, error) {
	base, err := url.Parse(config.URL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || (base.Scheme != "https" && !config.AllowPrivate) {
		return Result{}, fmt.Errorf("connector URL must be an absolute HTTPS URL")
	}
	if config.CursorParameter == "" != (config.NextCursorPointer == "") {
		return Result{}, fmt.Errorf("cursor parameter and next-cursor pointer must be configured together")
	}
	if err := validateTarget(ctx, base, config.AllowPrivate); err != nil {
		return Result{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = safeDialer(config.AllowPrivate)
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many connector redirects")
			}
			if !sameOrigin(base, request.URL) {
				return fmt.Errorf("connector redirect changed origin")
			}
			return validateTarget(request.Context(), request.URL, config.AllowPrivate)
		},
	}
	defer transport.CloseIdleConnections()
	current := *base
	var result Result
	for page := 0; page < maxPages; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return result, err
		}
		request.Header.Set("Accept", "application/json")
		if config.BearerToken != "" {
			request.Header.Set("Authorization", "Bearer "+config.BearerToken)
		}
		response, err := client.Do(request)
		if err != nil {
			return result, fmt.Errorf("fetch connector page: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxPageBytes+1))
		response.Body.Close()
		if readErr != nil {
			return result, fmt.Errorf("read connector page: %w", readErr)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			retryAfter, _ := strconv.Atoi(response.Header.Get("Retry-After"))
			return result, &HTTPError{Status: response.StatusCode, RetryAfter: time.Duration(retryAfter) * time.Second}
		}
		if len(body) > maxPageBytes {
			return result, fmt.Errorf("connector page exceeds 10 MiB")
		}
		var document any
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return result, fmt.Errorf("decode connector JSON: %w", err)
		}
		records, err := pointer(document, config.RecordsPointer)
		if err != nil {
			return result, fmt.Errorf("records pointer: %w", err)
		}
		items, ok := records.([]any)
		if !ok {
			return result, fmt.Errorf("records pointer must select an array")
		}
		for _, item := range items {
			line, err := json.Marshal(item)
			if err != nil {
				return result, fmt.Errorf("encode connector record: %w", err)
			}
			if result.Bytes+int64(len(line)+1) > maxTotal {
				return result, fmt.Errorf("connector result exceeds 100 MiB")
			}
			if _, err := output.Write(append(line, '\n')); err != nil {
				return result, err
			}
			result.Rows++
			result.Bytes += int64(len(line) + 1)
		}
		if config.CursorParameter == "" {
			return result, nil
		}
		next, err := pointer(document, config.NextCursorPointer)
		if err != nil || next == nil || fmt.Sprint(next) == "" {
			return result, nil
		}
		query := current.Query()
		query.Set(config.CursorParameter, fmt.Sprint(next))
		current.RawQuery = query.Encode()
	}
	return result, fmt.Errorf("connector pagination exceeds %d pages", maxPages)
}

func pointer(value any, path string) (any, error) {
	if path == "" {
		return value, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("JSON Pointer must be empty or begin with /")
	}
	current := value
	for _, raw := range strings.Split(path[1:], "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[part]
			if !ok {
				return nil, fmt.Errorf("segment %q was not found", part)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("segment %q is not an array index", part)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("segment %q traverses a scalar", part)
		}
	}
	return current, nil
}

func safeDialer(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if allowPrivate || publicIP(address.IP) {
				return dialer.DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
			}
		}
		return nil, fmt.Errorf("connector target resolves only to private or reserved addresses")
	}
}

func validateTarget(ctx context.Context, target *url.URL, allowPrivate bool) error {
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("connector target has no host")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve connector target: %w", err)
	}
	for _, address := range addresses {
		if !allowPrivate && !publicIP(address.IP) {
			return fmt.Errorf("connector target resolves to a private or reserved address")
		}
	}
	return nil
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func Retryable(err error) (time.Duration, bool) {
	var response *HTTPError
	if errors.As(err, &response) {
		return response.RetryAfter, response.Retryable()
	}
	var network net.Error
	return 0, errors.As(err, &network)
}
