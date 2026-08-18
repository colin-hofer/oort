package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string
}

type Client struct {
	config Config
	http   *http.Client
	now    func() time.Time
}

func New(config Config) (*Client, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("S3 endpoint must be an HTTP or HTTPS URL")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return nil, fmt.Errorf("S3 endpoint must not contain a path")
	}
	if config.Region == "" || config.AccessKey == "" || config.SecretKey == "" || config.Bucket == "" {
		return nil, fmt.Errorf("S3 region, credentials, and bucket are required")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	return &Client{config: config, http: &http.Client{Timeout: 15 * time.Minute}, now: time.Now}, nil
}

func (c *Client) PresignPut(key string, size int64, lifetime time.Duration) (string, http.Header, error) {
	if size < 0 || lifetime <= 0 || lifetime > 7*24*time.Hour {
		return "", nil, fmt.Errorf("invalid signed upload size or lifetime")
	}
	target, err := c.objectURL(key)
	if err != nil {
		return "", nil, err
	}
	now := c.now().UTC()
	date, dateTime := now.Format("20060102"), now.Format("20060102T150405Z")
	headers := http.Header{"Content-Length": []string{strconv.FormatInt(size, 10)}}
	signedHeaders, canonicalHeaders := canonicalHeaders(target.Host, headers)
	scope := date + "/" + c.config.Region + "/s3/aws4_request"
	query := target.Query()
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", c.config.AccessKey+"/"+scope)
	query.Set("X-Amz-Date", dateTime)
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(lifetime/time.Second), 10))
	query.Set("X-Amz-SignedHeaders", signedHeaders)
	target.RawQuery = query.Encode()
	canonical := strings.Join([]string{
		http.MethodPut, target.EscapedPath(), target.RawQuery, canonicalHeaders,
		signedHeaders, "UNSIGNED-PAYLOAD",
	}, "\n")
	target.RawQuery += "&X-Amz-Signature=" + c.signature(date, scope, dateTime, canonical)
	return target.String(), headers, nil
}

func (c *Client) Download(ctx context.Context, key string, output io.Writer, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, fmt.Errorf("download limit must be non-negative")
	}
	target, err := c.objectURL(key)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	c.sign(request)
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("download staged object: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download staged object: S3 returned HTTP %d", response.StatusCode)
	}
	written, err := io.Copy(output, io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return written, fmt.Errorf("download staged object: %w", err)
	}
	if written > maxBytes {
		return written, fmt.Errorf("staged object exceeds %d bytes", maxBytes)
	}
	return written, nil
}

func (c *Client) HeadBucket(ctx context.Context) error {
	target, err := c.bucketURL()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return err
	}
	c.sign(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("S3 returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *Client) DataPath(tenantID string) string {
	return "s3://" + c.config.Bucket + "/tenants/" + tenantID + "/lake/"
}

func (c *Client) Config() Config { return c.config }

func (c *Client) sign(request *http.Request) {
	now := c.now().UTC()
	date, dateTime := now.Format("20060102"), now.Format("20060102T150405Z")
	payload := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(payload[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	request.Header.Set("X-Amz-Date", dateTime)
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	signedHeaders, headers := canonicalHeaders(host, request.Header)
	canonical := strings.Join([]string{
		request.Method, request.URL.EscapedPath(), request.URL.Query().Encode(), headers,
		signedHeaders, payloadHash,
	}, "\n")
	scope := date + "/" + c.config.Region + "/s3/aws4_request"
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.config.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+c.signature(date, scope, dateTime, canonical))
}

func (c *Client) signature(date, scope, dateTime, canonical string) string {
	canonicalHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + dateTime + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	key := hmacSHA256([]byte("AWS4"+c.config.SecretKey), date)
	key = hmacSHA256(key, c.config.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

func (c *Client) objectURL(key string) (*url.URL, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return nil, fmt.Errorf("invalid object key")
	}
	target, _ := url.Parse(c.config.Endpoint)
	parts := append([]string{c.config.Bucket}, strings.Split(key, "/")...)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid object key")
		}
	}
	escaped := make([]string, len(parts))
	for index, part := range parts {
		escaped[index] = url.PathEscape(part)
	}
	target.Path = "/" + strings.Join(parts, "/")
	target.RawPath = "/" + strings.Join(escaped, "/")
	return target, nil
}

func (c *Client) bucketURL() (*url.URL, error) {
	target, _ := url.Parse(c.config.Endpoint)
	target.Path = "/" + c.config.Bucket
	target.RawPath = "/" + url.PathEscape(c.config.Bucket)
	return target, nil
}

func canonicalHeaders(host string, headers http.Header) (string, string) {
	values := map[string]string{"host": host}
	for name, entries := range headers {
		lower := strings.ToLower(name)
		if lower == "authorization" || (lower != "content-length" && !strings.HasPrefix(lower, "x-amz-")) {
			continue
		}
		cleaned := make([]string, len(entries))
		for index, entry := range entries {
			cleaned[index] = strings.Join(strings.Fields(entry), " ")
		}
		values[lower] = strings.Join(cleaned, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteByte(':')
		canonical.WriteString(values[name])
		canonical.WriteByte('\n')
	}
	return strings.Join(names, ";"), canonical.String()
}

func hmacSHA256(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}
