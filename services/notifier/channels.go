package notifier

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Secrets resolves the secret references of channel configs (spec 15.4
// "sm://..."): platform.Secrets, wired in cmd as platform.LocalSecrets
// (env://, file://) plus the Secret Manager adapter for sm://. Values never
// leave the process except to the channel they belong to.
type Secrets = platform.Secrets

// ErrSecretRef reports an unsupported or malformed reference.
var ErrSecretRef = platform.ErrSecretRef

// MapSecrets is an in-memory Secrets (tests and the dev stack).
type MapSecrets = platform.MemSecrets

// NewMapSecrets returns a resolver over m.
func NewMapSecrets(m map[string]string) *MapSecrets { return platform.NewMemSecrets(m) }

// Message is one delivery's content.
type Message struct {
	DeliveryID string
	AlertID    string
	Severity   string
	Summary    string
	// Body is the signed webhook JSON (alerts.AlertBody, or a digest).
	Body []byte
	At   time.Time
}

// Sender delivers to one channel kind. The status code is informative;
// an error means the attempt failed and is retried.
type Sender interface {
	Send(ctx context.Context, ch Channel, target string, m Message) (int, error)
}

// EmailSender is the e-mail port.
type EmailSender interface {
	SendMail(ctx context.Context, cfg map[string]any, from, to, subject, text string) error
}

// LogEmail logs e-mails instead of sending them (dev).
type LogEmail struct{ Logger *slog.Logger }

// SendMail implements EmailSender.
func (l LogEmail) SendMail(ctx context.Context, _ map[string]any, from, to, subject, _ string) error {
	l.Logger.InfoContext(ctx, "notifier.email_logged", "from", from, "to", to, "subject", subject)
	return nil
}

// SMTPEmail sends through net/smtp. The channel's provider_secret_ref
// resolves to "user:password@host:port". net/smtp takes no context, so
// cancelling ctx does not interrupt a send in progress.
type SMTPEmail struct{ Secrets Secrets }

// SendMail implements EmailSender.
func (s SMTPEmail) SendMail(ctx context.Context, cfg map[string]any, from, to, subject, text string) error {
	ref, _ := cfg["provider_secret_ref"].(string)
	sec, err := s.Secrets.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	cred, addr, ok := strings.Cut(string(sec), "@")
	user, pass, _ := strings.Cut(cred, ":")
	if !ok {
		return errors.New("notifier: smtp secret must be user:password@host:port")
	}
	host, _, _ := net.SplitHostPort(addr)
	from, to = headerValue(from), headerValue(to)
	return smtp.SendMail(addr, smtp.PlainAuth("", user, pass, host), from, []string{to}, SMTPMessage(from, to, subject, text))
}

// SMTPMessage builds the RFC 5322 message SMTPEmail sends. Header values
// have CR, LF and other control characters replaced with spaces: the
// subject carries the alert summary, which a rule renders from graph data
// (titles, names) a scanned target controls, and a line break there would
// let it add headers or a body of its own (G-SVC-19). Body lines end in
// CRLF as SMTP requires.
func SMTPMessage(from, to, subject, text string) []byte {
	body := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	return []byte("From: " + headerValue(from) + "\r\nTo: " + headerValue(to) + "\r\nSubject: " + headerValue(subject) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body)
}

// headerValue replaces control characters (CR and LF included) with
// spaces, so a value stays on its header line.
func headerValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, v)
}

// Senders holds the channel implementations.
type Senders struct {
	Secrets  Secrets
	Resolver scope.Resolver // webhook destination resolution
	Email    EmailSender
	// AllowInsecureLoopback permits http://127.0.0.1 webhook destinations
	// (tests only). Everything else must be https to a public address.
	AllowInsecureLoopback bool
	// Egress, when set, sends webhook and Slack deliveries through the
	// egress proxy's notify path (spec 17.1: the proxy is the only
	// identity with internet access; G-SVC-08). The proxy then resolves,
	// SSRF-checks and pins the destination and applies the operator
	// allowlist. Nil sends directly after the same SSRF checks here (local
	// development; prod requires FF_EGRESS_PROXY).
	Egress *NotifyEgress
}

// NotifyEgress is the route of deliveries through the egress proxy.
type NotifyEgress struct {
	Proxy  *neturl.URL
	Tokens platform.TokenSource
	// IDTokens adds the Google ID token Cloud Run's invoker check needs
	// when the proxy is an https:// Cloud Run URL.
	IDTokens platform.IDTokenSource
	// TLS overrides the TLS configuration (proxy hop and destination;
	// tests); nil means the system roots.
	TLS *tls.Config
}

// NewNotifyEgress parses the proxy address (host:port, http://host:port or
// https://host[:port]); an empty address returns nil (direct delivery).
func NewNotifyEgress(addr string, tokens platform.TokenSource, ids platform.IDTokenSource) (*NotifyEgress, error) {
	if addr == "" {
		return nil, nil
	}
	u, err := neturl.Parse(addr)
	if err != nil || u.Host == "" {
		u, err = neturl.Parse("http://" + addr)
	}
	if err != nil {
		return nil, fmt.Errorf("notifier: egress proxy address %q: %w", addr, err)
	}
	return &NotifyEgress{Proxy: u, Tokens: tokens, IDTokens: ids}, nil
}

// client returns a client whose https connections are CONNECT tunnels
// through the proxy's notify path, authenticated as the notifier for
// tenant. Redirects are not followed (as for direct delivery).
func (e *NotifyEgress) client(tenant core.UUID) *http.Client {
	return &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: http.ProxyURL(e.Proxy), TLSClientConfig: e.TLS,
			GetProxyConnectHeader: func(ctx context.Context, _ *neturl.URL, _ string) (http.Header, error) {
				h := http.Header{}
				h.Set(platform.HeaderEgressPurpose, platform.PurposeNotify)
				if e.Tokens != nil {
					tok, err := e.Tokens.Token(ctx, platform.Identity{Tenant: tenant, Actor: platform.ServiceActor("notifier"), Role: auth.RoleAnalyst})
					if err != nil {
						return nil, err
					}
					h.Set(platform.HeaderProxyAuthorization, "Bearer "+tok)
				}
				if e.Proxy.Scheme == "https" && e.IDTokens != nil {
					tok, err := e.IDTokens.IDToken(ctx, "https://"+e.Proxy.Hostname())
					if err != nil {
						return nil, err
					}
					h.Set(platform.MDServerlessAuthorization, "Bearer "+tok)
				}
				return h, nil
			}}}
}

// Send dispatches on the channel kind.
func (s *Senders) Send(ctx context.Context, ch Channel, target string, m Message) (int, error) {
	switch ch.Kind {
	case KindWebhook:
		return s.webhook(ctx, ch, m)
	case KindSlack:
		return s.slack(ctx, ch, target, m)
	case KindEmail:
		return s.email(ctx, ch, target, m)
	}
	return 0, fmt.Errorf("notifier: unknown channel kind %q", ch.Kind)
}

// checkedClient resolves raw's host, checks every address against the
// SSRF deny list (spec 15.4) and returns a client pinned to them. With
// Egress set, the URL rules are checked here and the address checks and
// pinning happen in the proxy.
func (s *Senders) checkedClient(ctx context.Context, tenant core.UUID, raw string) (*http.Client, error) {
	u, err := alerts.ValidateWebhookURL(raw)
	var addrs []netip.Addr
	if err == nil && s.Egress != nil {
		return s.Egress.client(tenant), nil
	}
	if err != nil {
		pu, perr := neturl.Parse(raw)
		if !s.AllowInsecureLoopback || perr != nil || pu.Scheme != "http" || pu.Hostname() != "127.0.0.1" || pu.User != nil || pu.Port() == "" {
			return nil, err
		}
		return pinnedClient([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, pu.Port()), nil
	}
	host := u.Host.String()
	if a, perr := netip.ParseAddr(strings.Trim(host, "[]")); perr == nil {
		addrs = []netip.Addr{a}
	} else {
		if s.Resolver == nil {
			return nil, errors.New("notifier: no resolver for webhook destinations")
		}
		if addrs, err = s.Resolver.LookupAddrs(ctx, host); err != nil {
			return nil, err
		}
	}
	if err := alerts.CheckResolved(addrs); err != nil {
		return nil, err
	}
	port := "443"
	if u.Port != 0 {
		port = strconv.Itoa(u.Port)
	}
	return pinnedClient(addrs, port), nil
}

func pinnedClient(addrs []netip.Addr, port string) *http.Client {
	return &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var last error
				for _, a := range addrs {
					var d net.Dialer
					c, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
					if err == nil {
						return c, nil
					}
					last = err
				}
				return nil, last
			}}}
}

func post(ctx context.Context, c *http.Client, url string, body []byte, headers map[string]string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("notifier: destination answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (s *Senders) webhook(ctx context.Context, ch Channel, m Message) (int, error) {
	url, _ := ch.Config["url"].(string)
	ref, _ := ch.Config["secret_ref"].(string)
	secret, err := s.Secrets.Resolve(ctx, ref)
	if err != nil {
		return 0, err
	}
	c, err := s.checkedClient(ctx, ch.Tenant, url)
	if err != nil {
		return 0, err
	}
	h, err := alerts.WebhookHeaders(secret, m.DeliveryID, m.At, m.Body)
	if err != nil {
		return 0, err
	}
	return post(ctx, c, url, m.Body, h)
}

func (s *Senders) slack(ctx context.Context, ch Channel, target string, m Message) (int, error) {
	ref, _ := ch.Config["webhook_secret_ref"].(string)
	hook, err := s.Secrets.Resolve(ctx, ref)
	if err != nil {
		return 0, err
	}
	c, err := s.checkedClient(ctx, ch.Tenant, string(hook))
	if err != nil {
		return 0, err
	}
	channel := target
	if channel == "" {
		channel, _ = ch.Config["default_channel"].(string)
	}
	body, _ := json.Marshal(map[string]any{"channel": channel, "text": fmt.Sprintf("[%s] %s", strings.ToUpper(m.Severity), m.Summary)})
	return post(ctx, c, string(hook), body, nil)
}

func (s *Senders) email(ctx context.Context, ch Channel, target string, m Message) (int, error) {
	if s.Email == nil {
		return 0, errors.New("notifier: no e-mail sender configured")
	}
	from, _ := ch.Config["from"].(string)
	if target == "" {
		return 0, errors.New("notifier: e-mail action without a recipient")
	}
	text := m.Summary + "\n\nSeverity: " + m.Severity + "\nAlert: " + m.AlertID + "\n"
	return 0, s.Email.SendMail(ctx, ch.Config, from, target, "[Flowforge] "+m.Summary, text)
}
