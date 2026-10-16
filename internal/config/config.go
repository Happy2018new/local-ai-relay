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
	UpstreamAPIKey        string   `json:"upstream_api_key"`
	LocalAPIKey           string   `json:"local_api_key"`
	UpstreamAPIKeyEnv     string   `json:"upstream_api_key_env"`
	LocalAPIKeyEnv        string   `json:"local_api_key_env"`
	LogFile               string   `json:"log_file,omitempty"`

	// Legacy references are read only for compatibility and migration.
	UpstreamAPIKeyRef string `json:"upstream_api_key_ref,omitempty"`
	LocalAPIKeyRef    string `json:"local_api_key_ref,omitempty"`
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
	for _, sources := range [][3]string{
		{c.UpstreamAPIKey, c.UpstreamAPIKeyEnv, c.UpstreamAPIKeyRef},
		{c.LocalAPIKey, c.LocalAPIKeyEnv, c.LocalAPIKeyRef},
	} {
		count := 0
		for _, source := range sources {
			if source != "" {
				count++
			}
		}
		if count > 1 {
			return errors.New("choose one key source (config, environment, or legacy credential reference) for each API key")
		}
		if sources[0] != "" {
			if err := validateKey(sources[0]); err != nil {
				return err
			}
		}
	}
	if (c.LocalAPIKey != "" || c.LocalAPIKeyEnv != "" || c.LocalAPIKeyRef != "") && c.UpstreamAPIKey == "" && c.UpstreamAPIKeyEnv == "" && c.UpstreamAPIKeyRef == "" {
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

func (c Config) ResolveSecrets(store credentials.Reader) (Secrets, error) {
	var secrets Secrets
	if err := c.Validate(); err != nil {
		return secrets, err
	}
	for _, item := range []struct {
		value string
		name  string
		ref   string
		dest  *string
	}{
		{c.UpstreamAPIKey, c.UpstreamAPIKeyEnv, c.UpstreamAPIKeyRef, &secrets.UpstreamKey},
		{c.LocalAPIKey, c.LocalAPIKeyEnv, c.LocalAPIKeyRef, &secrets.LocalKey},
	} {
		if item.value == "" && item.name == "" && item.ref == "" {
			continue
		}
		value, err := resolveKey(item.value, item.name, item.ref, store)
		if err != nil {
			return Secrets{}, err
		}
		*item.dest = value
	}
	return secrets, nil
}

// InlineLegacyKeys copies old credential references into editable config fields.
// It never writes to or deletes entries from the system credential store.
func (c Config) InlineLegacyKeys(store credentials.Reader) (Config, error) {
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	for _, item := range []struct{ value, reference *string }{
		{&c.UpstreamAPIKey, &c.UpstreamAPIKeyRef},
		{&c.LocalAPIKey, &c.LocalAPIKeyRef},
	} {
		if *item.reference == "" {
			continue
		}
		value, err := resolveKey("", "", *item.reference, store)
		if err != nil {
			return Config{}, err
		}
		*item.value, *item.reference = value, ""
	}
	return c, nil
}

func resolveKey(value, env, reference string, store credentials.Reader) (string, error) {
	if reference != "" {
		var err error
		if store == nil {
			err = errors.New("legacy credential reader unavailable")
		} else {
			value, err = store.Get(reference)
		}
		if err != nil {
			return "", errors.New("cannot read legacy API key; enter the key again in the TUI or replace the *_api_key_ref field with *_api_key in config")
		}
	} else if env != "" {
		value = os.Getenv(env)
	}
	if err := validateKey(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func validateKey(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("configured API key is empty")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("configured API key contains NUL or newline characters")
	}
	return nil
}
