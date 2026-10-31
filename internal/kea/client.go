// Package kea is a client for the direct HTTP(S) control socket of kea-dhcp4.
package kea

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Result codes of the Kea management API.
const (
	ResultSuccess     = 0
	ResultError       = 1
	ResultUnsupported = 2
	ResultEmpty       = 3
	ResultConflict    = 4
)

// Kea guards control connections with a fixed 10s timeout; the client allows a
// little more so that the server side timeout is the one that gets reported.
const requestTimeout = 15 * time.Second

const maxResponseSize = 64 << 20

// UnreachableError means no valid answer arrived. For a write command the state
// of the server is unknown afterwards.
type UnreachableError struct {
	Err error
}

func (e *UnreachableError) Error() string { return "Kea unreachable: " + e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// HTTPError is a non-200 answer of the control socket, e.g. failed authentication.
type HTTPError struct {
	Status int
}

func (e *HTTPError) Error() string {
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return fmt.Sprintf("Kea denied access (HTTP %d), check the credentials", e.Status)
	}
	return fmt.Sprintf("Kea answered with HTTP %d", e.Status)
}

// CommandError is a Kea answer with a result code that signals failure.
type CommandError struct {
	Command string
	Code    int
	Text    string
}

func (e *CommandError) Error() string {
	if e.Code == ResultUnsupported {
		return fmt.Sprintf("command %s is not supported (hook library not loaded?)", e.Command)
	}
	return fmt.Sprintf("%s failed (code %d): %s", e.Command, e.Code, e.Text)
}

// IsUnsupported reports whether err is caused by a command Kea does not know,
// which usually means the providing hook library is not loaded.
func IsUnsupported(err error) bool {
	var ce *CommandError
	return errors.As(err, &ce) && ce.Code == ResultUnsupported
}

// Response is one decoded answer of the control socket.
type Response struct {
	Result    int             `json:"result"`
	Text      string          `json:"text"`
	Arguments json.RawMessage `json:"arguments"`
}

// Options configures a Client.
type Options struct {
	URL        string
	User       string
	Password   string
	CAFile     string
	ClientCert string
	ClientKey  string
}

// Client talks to one Kea instance.
type Client struct {
	url         string
	user        string
	password    string
	http        *http.Client
	lastContact atomic.Int64
}

// NewClient builds a Client; it does not contact the server.
func NewClient(o Options) (*Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no certificate", o.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	if o.ClientCert != "" {
		cert, err := tls.LoadX509KeyPair(o.ClientCert, o.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return &Client{
		url:      strings.TrimRight(o.URL, "/") + "/",
		user:     o.User,
		password: o.Password,
		http: &http.Client{
			Timeout:   requestTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsConfig, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
		},
	}, nil
}

// URL returns the configured control socket URL.
func (c *Client) URL() string { return c.url }

// LastContact returns the time of the last valid answer; zero if there was none.
func (c *Client) LastContact() time.Time {
	if ms := c.lastContact.Load(); ms != 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

// Do sends a command and returns the answer without interpreting the result code.
func (c *Client) Do(ctx context.Context, command string, args any) (*Response, error) {
	body, err := json.Marshal(struct {
		Command   string `json:"command"`
		Arguments any    `json:"arguments,omitempty"`
	}{command, args})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &UnreachableError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, &UnreachableError{Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	r, err := decodeResponse(data)
	if err != nil {
		return nil, &UnreachableError{Err: fmt.Errorf("invalid answer to %s: %w", command, err)}
	}
	c.lastContact.Store(time.Now().UnixMilli())
	return r, nil
}

// The HTTP socket wraps its answer in a one element list while the reference
// shows a bare object, so both are accepted.
func decodeResponse(data []byte) (*Response, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var list []Response
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, errors.New("empty answer list")
		}
		return &list[0], nil
	}
	var r Response
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// call sends a command and turns every result except the allowed ones into an error.
func (c *Client) call(ctx context.Context, command string, args any, allowed ...int) (*Response, error) {
	r, err := c.Do(ctx, command, args)
	if err != nil {
		return nil, err
	}
	if r.Result == ResultSuccess {
		return r, nil
	}
	for _, code := range allowed {
		if r.Result == code {
			return r, nil
		}
	}
	return nil, &CommandError{Command: command, Code: r.Result, Text: r.Text}
}

func decodeArguments(r *Response, command string, out any) error {
	if len(r.Arguments) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(r.Arguments))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("cannot read answer to %s: %w", command, err)
	}
	return nil
}
