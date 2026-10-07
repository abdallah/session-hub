package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/abdallah/session-hub/internal/paths"
)

// Config is the server configuration: server.toml, then env overrides.
type Config struct {
	Listen    []string
	PublicURL string
	// LegacyReadToken is set when server.toml has read_token or
	// SESSIONHUB_READ_TOKEN is set. Both are ignored; Run logs one warning.
	LegacyReadToken bool
	StaleAfter      time.Duration
	DB              string // from SESSIONHUB_DB (paths.DB); not a file key
	// TelegramBotToken and TelegramChatID turn on Telegram alerts for
	// Blocked inbox items. Both must be set. See docs/server.md, "Telegram
	// alerts".
	TelegramBotToken Secret
	TelegramChatID   string
	// TelegramProblem says why a Telegram setting in server.toml is unusable
	// (a wrong type). It never stops the server: alerts stay off and Run logs
	// it once. An env override that fixes the setting clears it.
	TelegramProblem string
	// TaskTicketURL and TaskMRURL link a note's ref: {ref} is the ticket
	// ref, {n} the merge request number. Empty means no link.
	TaskTicketURL string
	TaskMRURL     string
	// Warnings are problems that do not stop the server: unknown
	// telegram_* keys in server.toml, such as a misspelled name. Run logs
	// each once at startup.
	Warnings []string
}

// Secret is a setting that must never reach a log. It prints as
// [redacted], so a %v, %+v, or %#v of a Config is safe.
type Secret string

// String hides the value.
func (Secret) String() string { return "[redacted]" }

// MarshalText hides the value from JSON and other text encoders.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// GoString hides the value from %#v.
func (Secret) GoString() string { return `"[redacted]"` }

// AlertsEnabled reports whether both Telegram settings are set.
func (c Config) AlertsEnabled() bool { return c.TelegramBotToken != "" && c.TelegramChatID != "" }

// fileConfig mirrors server.toml.
type fileConfig struct {
	Listen     []string `toml:"listen"`
	PublicURL  string   `toml:"public_url"`
	ReadToken  string   `toml:"read_token"` // ignored; kept so an old file still loads
	StaleAfter string   `toml:"stale_after"`
	// Telegram alerts (docs/dev/superpowers/specs/2026-10-01-inbox-alerts-design.md).
	// Accepted now so a file that sets them loads; the notifier uses them.
	TelegramBotToken telegramString `toml:"telegram_bot_token"`
	TelegramChatID   telegramString `toml:"telegram_chat_id"`
	TaskTicketURL    string         `toml:"task_ticket_url"`
	TaskMRURL        string         `toml:"task_mr_url"`
}

// telegramString reads a Telegram setting written as a string ("-1001") or an
// integer (-1001). Any other type is recorded in problem instead of returned
// as an error, because a Telegram setting must never stop the server.
type telegramString struct {
	val     string
	problem string // "must be a string or an integer", or empty
}

// UnmarshalTOML implements toml.Unmarshaler.
func (c *telegramString) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		c.val = x
	case int64:
		c.val = strconv.FormatInt(x, 10)
	default:
		c.problem = "must be a string or an integer"
	}
	return nil
}

// Defaults for server.toml keys.
const (
	DefaultListen     = "127.0.0.1:8787"
	DefaultPublicURL  = "http://127.0.0.1:8787"
	DefaultStaleAfter = 5 * time.Minute
)

// LoadConfig reads paths.ServerConfig() (a missing file means defaults) and
// applies the SESSIONHUB_* env overrides. read_token is read only to warn about it.
func LoadConfig() (Config, error) {
	cfg := Config{
		Listen:     []string{DefaultListen},
		PublicURL:  DefaultPublicURL,
		StaleAfter: DefaultStaleAfter,
		DB:         paths.DB(),
	}
	path := paths.ServerConfig()
	var fc fileConfig
	md, err := toml.DecodeFile(path, &fc)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cfg, fmt.Errorf("%s: %w", path, err)
	default:
		// An unknown telegram_* key is a warning: a typo there must not stop
		// the server. Every other unknown key is still an error.
		for _, k := range md.Undecoded() {
			if strings.HasPrefix(k.String(), "telegram_") {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("%s: unknown key %q ignored; telegram alerts use telegram_bot_token and telegram_chat_id", path, k.String()))
				continue
			}
			return cfg, fmt.Errorf("%s: unknown key %q", path, k.String())
		}
	}
	if len(fc.Listen) > 0 {
		cfg.Listen = fc.Listen
	}
	if fc.PublicURL != "" {
		cfg.PublicURL = fc.PublicURL
	}
	cfg.LegacyReadToken = fc.ReadToken != ""
	cfg.TaskTicketURL, cfg.TaskMRURL = strings.TrimSpace(fc.TaskTicketURL), strings.TrimSpace(fc.TaskMRURL)
	for _, k := range []struct{ name, v string }{{"task_ticket_url", cfg.TaskTicketURL}, {"task_mr_url", cfg.TaskMRURL}} {
		if k.v == "" {
			continue
		}
		if u, err := url.Parse(k.v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return cfg, fmt.Errorf("%s: %s %q must be an absolute http or https URL with a host", path, k.name, k.v)
		}
	}
	cfg.TelegramBotToken = Secret(strings.TrimSpace(fc.TelegramBotToken.val))
	cfg.TelegramChatID = strings.TrimSpace(fc.TelegramChatID.val)
	tokenProblem, chatProblem := fc.TelegramBotToken.problem, fc.TelegramChatID.problem
	if fc.StaleAfter != "" {
		if cfg.StaleAfter, err = parseStaleAfter(fc.StaleAfter); err != nil {
			return cfg, fmt.Errorf("%s: stale_after: %w", path, err)
		}
	}

	if v := os.Getenv("SESSIONHUB_LISTEN"); v != "" {
		cfg.Listen = nil
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				cfg.Listen = append(cfg.Listen, a)
			}
		}
		if len(cfg.Listen) == 0 {
			return cfg, errors.New("SESSIONHUB_LISTEN has no addresses")
		}
	}
	if v := os.Getenv("SESSIONHUB_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if os.Getenv("SESSIONHUB_READ_TOKEN") != "" {
		cfg.LegacyReadToken = true
	}
	if v := strings.TrimSpace(os.Getenv("SESSIONHUB_TELEGRAM_BOT_TOKEN")); v != "" {
		cfg.TelegramBotToken = Secret(v)
		tokenProblem = ""
	}
	if v := strings.TrimSpace(os.Getenv("SESSIONHUB_TELEGRAM_CHAT_ID")); v != "" {
		cfg.TelegramChatID = v
		chatProblem = ""
	}
	switch {
	case tokenProblem != "":
		cfg.TelegramProblem = "telegram_bot_token " + tokenProblem
	case chatProblem != "":
		cfg.TelegramProblem = "telegram_chat_id " + chatProblem
	}
	if v := os.Getenv("SESSIONHUB_STALE_AFTER"); v != "" {
		if cfg.StaleAfter, err = parseStaleAfter(v); err != nil {
			return cfg, fmt.Errorf("SESSIONHUB_STALE_AFTER: %w", err)
		}
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if u, err := url.Parse(cfg.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return cfg, fmt.Errorf("public_url (or SESSIONHUB_PUBLIC_URL) %q must be an absolute http or https URL with a host", cfg.PublicURL)
	}
	return cfg, nil
}

func parseStaleAfter(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be positive", s)
	}
	return d, nil
}
