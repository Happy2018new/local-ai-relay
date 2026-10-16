package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/credentials"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("duration must be a string such as 1s or 3m")
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(value)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Value().String())
}

type Config struct {
	Listen                string   `json:"listen"`
	UpstreamURL           string   `json:"upstream_url"`
	MaxConcurrent         int      `json:"max_concurrent"`
	MaxQueue              int      `json:"max_queue"`
	QueueTimeout          Duration `json:"queue_timeout"`
	MinInterval           Duration `json:"min_interval"`
	MaxAttempts           int      `json:"max_attempts"`
	RetryWindow           Duration `json:"retry_window"`
	InitialBackoff        Duration `json:"initial_backoff"`
	MaxBackoff            Duration `json:"max_backoff"`
	ResponseHeaderTimeout Duration `json:"response_header_timeout"`
	StreamIdleTimeout     Duration `json:"stream_idle_timeout"`
	ErrorBodyTimeout      Duration `json:"error_body_timeout"`
	MaxRequestBytes       int64    `json:"max_request_bytes"`
	RequireStream         bool     `json:"require_stream"`
	UpstreamAPIKeyEnv     string   `json:"upstream_api_key_env"`
	LocalAPIKeyEnv        string   `json:"local_api_key_env"`
	UpstreamAPIKeyRef     string   `json:"upstream_api_key_ref,omitempty"`
	LocalAPIKeyRef        string   `json:"local_api_key_ref,omitempty"`
	LogFile               string   `json:"log_file,omitempty"`
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8787", MaxConcurrent: 1, MaxQueue: 32,
		QueueTimeout: Duration(3 * time.Minute), MinInterval: Duration(time.Second),
		MaxAttempts: 5, RetryWindow: Duration(3 * time.Minute),
		InitialBackoff: Duration(2 * time.Second), MaxBackoff: Duration(30 * time.Second),
		ResponseHeaderTimeout: Duration(5 * time.Minute),
		StreamIdleTimeout:     Duration(5 * time.Minute), ErrorBodyTimeout: Duration(10 * time.Second),
		MaxRequestBytes: 64 << 20, RequireStream: true,
	}
}

func Load(reader io.Reader) (Config, error) {
	c := Default()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || port == "" {
		return errors.New("listen must be a loopback host:port, for example 127.0.0.1:8787")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("listen port must be an integer from 0 to 65535")
	}
	if strings.ContainsAny(c.LogFile, "\x00\r\n") {
		return errors.New("log_file must not contain NUL or newline characters")
	}
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("listen must use localhost or a loopback IP address")
	}
	upstream, err := url.Parse(c.UpstreamURL)
	if err != nil || upstream.Hostname() == "" || (upstream.Scheme != "https" && upstream.Scheme != "http") {
		return errors.New("upstream_url must be an absolute http or https URL")
	}
	if upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.RawPath != "" {
		return errors.New("upstream_url must not contain credentials, query parameters, fragments, or escaped paths")
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 1024 || c.MaxQueue < 0 || c.MaxQueue > 100000 {
		return errors.New("max_concurrent must be 1..1024 and max_queue must be 0..100000")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 100 {
		return errors.New("max_attempts must be 1..100 (including the initial attempt)")
	}
	if c.QueueTimeout <= 0 || c.RetryWindow <= 0 || c.InitialBackoff <= 0 || c.MaxBackoff < c.InitialBackoff {
		return errors.New("queue_timeout, retry_window and initial_backoff must be positive; max_backoff must be >= initial_backoff")
	}
	if c.MinInterval < 0 || c.StreamIdleTimeout < 0 || c.ResponseHeaderTimeout <= 0 || c.ErrorBodyTimeout <= 0 {
		return errors.New("timeouts must be positive; min_interval and stream_idle_timeout may be zero")
	}
	if c.MaxRequestBytes < 1 || c.MaxRequestBytes > 1<<30 {
		return errors.New("max_request_bytes must be 1..1073741824")
	}
	if c.UpstreamAPIKeyEnv != "" && c.UpstreamAPIKeyRef != "" || c.LocalAPIKeyEnv != "" && c.LocalAPIKeyRef != "" {
		return errors.New("choose one key source (environment or credential store) for each API key")
	}
	if (c.LocalAPIKeyEnv != "" || c.LocalAPIKeyRef != "") && c.UpstreamAPIKeyEnv == "" && c.UpstreamAPIKeyRef == "" {
		return errors.New("local authentication requires a separate upstream key so local credentials are never forwarded")
	}
	return nil
}

type Secrets struct {
	UpstreamKey string
	LocalKey    string
}

func (c Config) Secrets() (Secrets, error) {
	return c.ResolveSecrets(credentials.System{})
}

func (c Config) ResolveSecrets(store credentials.Store) (Secrets, error) {
	var secrets Secrets
	if err := c.Validate(); err != nil {
		return secrets, err
	}
	for _, item := range []struct {
		name string
		ref  string
		dest *string
	}{{c.UpstreamAPIKeyEnv, c.UpstreamAPIKeyRef, &secrets.UpstreamKey}, {c.LocalAPIKeyEnv, c.LocalAPIKeyRef, &secrets.LocalKey}} {
		var value string
		if item.ref != "" {
			var err error
			value, err = store.Get(item.ref)
			if err != nil {
				return secrets, fmt.Errorf("cannot read credential %q from the system credential store", item.ref)
			}
		} else if item.name != "" {
			value = os.Getenv(item.name)
		} else {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return secrets, fmt.Errorf("configured API key is empty (environment %q, credential %q)", item.name, item.ref)
		}
		if strings.ContainsAny(value, "\r\n") {
			return secrets, errors.New("configured API key contains a newline")
		}
		*item.dest = value
	}
	return secrets, nil
}
