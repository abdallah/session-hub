package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func configEnv(t *testing.T, toml string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "server.toml")
	if toml != "" {
		if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SESSIONHUB_SERVER_CONFIG", path)
	t.Setenv("SESSIONHUB_DB", filepath.Join(dir, "sessionhub.db"))
	for _, k := range []string{"SESSIONHUB_LISTEN", "SESSIONHUB_PUBLIC_URL", "SESSIONHUB_READ_TOKEN", "SESSIONHUB_STALE_AFTER",
		"SESSIONHUB_TELEGRAM_BOT_TOKEN", "SESSIONHUB_TELEGRAM_CHAT_ID"} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig(t *testing.T) {
	const file = `
listen = ["127.0.0.1:9000", "100.64.0.1:9000"]
public_url = "https://sessionhub.example.test/"
read_token = "hub_r_fromfile"
stale_after = "90s"
`
	tests := []struct {
		name string
		toml string
		env  map[string]string
		want Config
	}{
		{"defaults when the file is missing", "", nil,
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "http://127.0.0.1:8787", StaleAfter: 5 * time.Minute}},
		{"file values", file, nil,
			Config{Listen: []string{"127.0.0.1:9000", "100.64.0.1:9000"}, PublicURL: "https://sessionhub.example.test",
				LegacyReadToken: true, StaleAfter: 90 * time.Second}},
		{"env overrides file", file, map[string]string{
			"SESSIONHUB_LISTEN": " 127.0.0.1:1 , ,[::1]:2", "SESSIONHUB_PUBLIC_URL": "http://localhost:1",
			"SESSIONHUB_READ_TOKEN": "hub_r_env", "SESSIONHUB_STALE_AFTER": "2s"},
			Config{Listen: []string{"127.0.0.1:1", "[::1]:2"}, PublicURL: "http://localhost:1",
				LegacyReadToken: true, StaleAfter: 2 * time.Second}},
		{"SESSIONHUB_READ_TOKEN alone", "", map[string]string{"SESSIONHUB_READ_TOKEN": "hub_r_env"},
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "http://127.0.0.1:8787", StaleAfter: 5 * time.Minute,
				LegacyReadToken: true}},
		{"telegram from the file", "telegram_bot_token = \" 123456:TEST-PLACEHOLDER-TOKEN \"\ntelegram_chat_id = \"-1001\"\n", nil,
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "http://127.0.0.1:8787", StaleAfter: 5 * time.Minute,
				TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "-1001"}},
		{"telegram chat id as an integer", "telegram_chat_id = -1001\n", nil,
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "http://127.0.0.1:8787", StaleAfter: 5 * time.Minute,
				TelegramChatID: "-1001"}},
		{"telegram env overrides file", "telegram_bot_token = \"file\"\ntelegram_chat_id = \"1\"\n", map[string]string{
			"SESSIONHUB_TELEGRAM_BOT_TOKEN": "123456:TEST-PLACEHOLDER-TOKEN", "SESSIONHUB_TELEGRAM_CHAT_ID": "42"},
			Config{Listen: []string{"127.0.0.1:8787"}, PublicURL: "http://127.0.0.1:8787", StaleAfter: 5 * time.Minute,
				TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configEnv(t, tt.toml)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			tt.want.DB = os.Getenv("SESSIONHUB_DB")
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigPublicURLTrailingSlash(t *testing.T) {
	configEnv(t, "")
	t.Setenv("SESSIONHUB_PUBLIC_URL", "https://sessionhub.example.com/")
	cfg, err := LoadConfig()
	if err != nil || cfg.PublicURL != "https://sessionhub.example.com" {
		t.Errorf("got %q, %v; want https://sessionhub.example.com and no error", cfg.PublicURL, err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		toml string
		env  map[string]string
		want string
	}{
		{"unknown key", `listen = ["x:1"]` + "\nread_tokn = \"typo\"\n", nil, `unknown key "read_tokn"`},
		{"syntax error", `listen = [`, nil, "server.toml"},
		{"bad stale_after", `stale_after = "soon"`, nil, "stale_after"},
		{"negative stale_after", `stale_after = "-1m"`, nil, "must be positive"},
		{"bad SESSIONHUB_STALE_AFTER", "", map[string]string{"SESSIONHUB_STALE_AFTER": "0s"}, "SESSIONHUB_STALE_AFTER"},
		{"public_url without scheme", `public_url = "sessionhub.example.com"`, nil, "public_url"},
		{"public_url slash only", "", map[string]string{"SESSIONHUB_PUBLIC_URL": "/"}, "SESSIONHUB_PUBLIC_URL"},
		{"public_url without host", `public_url = "https://"`, nil, "public_url"},
		{"empty SESSIONHUB_LISTEN list", "", map[string]string{"SESSIONHUB_LISTEN": " , "}, "SESSIONHUB_LISTEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configEnv(t, tt.toml)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A Telegram setting never stops the server: a wrong type is a recorded
// problem, a misspelled telegram_* key is a warning, and SESSIONHUB_TELEGRAM_CHAT_ID
// still overrides the file.
func TestLoadConfigTelegramProblems(t *testing.T) {
	tests := []struct {
		name, toml string
		env        map[string]string
		problem    string
		warnings   int
	}{
		{"array chat id", "telegram_chat_id = [42]\n", nil, "telegram_chat_id must be a string or an integer", 0},
		{"float chat id", "telegram_chat_id = 4.5\n", nil, "telegram_chat_id must be a string or an integer", 0},
		{"bool chat id", "telegram_chat_id = true\n", nil, "telegram_chat_id must be a string or an integer", 0},
		{"array token", "telegram_bot_token = [1]\n", nil, "telegram_bot_token must be a string or an integer", 0},
		{"env overrides a bad chat id", "telegram_chat_id = [42]\n", map[string]string{"SESSIONHUB_TELEGRAM_CHAT_ID": "42"}, "", 0},
		{"misspelled key", "telegram_chat_idd = \"1\"\n", nil, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configEnv(t, tt.toml)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig stopped the server: %v", err)
			}
			if cfg.TelegramProblem != tt.problem || len(cfg.Warnings) != tt.warnings {
				t.Errorf("problem %q, warnings %q; want problem %q and %d warnings", cfg.TelegramProblem, cfg.Warnings, tt.problem, tt.warnings)
			}
			if cfg.AlertsEnabled() && tt.problem != "" {
				t.Error("alerts enabled despite the problem")
			}
		})
	}
}

// A server.toml with the Telegram alert settings loads (they caused
// "unknown key" before the notifier existed).
func TestLoadConfigAcceptsTelegramKeys(t *testing.T) {
	configEnv(t, "telegram_bot_token = \"123:placeholder\"\ntelegram_chat_id = \"42\"\n")
	if _, err := LoadConfig(); err != nil {
		t.Errorf("LoadConfig: %v", err)
	}
}

func TestConfigHidesTelegramToken(t *testing.T) {
	cfg := Config{TelegramBotToken: "123456:TEST-PLACEHOLDER-TOKEN", TelegramChatID: "42"}
	for _, s := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprint(cfg.TelegramBotToken)} {
		if strings.Contains(s, "TEST-PLACEHOLDER") {
			t.Errorf("the token is printed: %s", s)
		}
	}
	if b, err := json.Marshal(cfg); err != nil || strings.Contains(string(b), "TEST-PLACEHOLDER") {
		t.Errorf("the token is in the JSON (err %v): %s", err, b)
	}
	if !cfg.AlertsEnabled() {
		t.Error("alerts off with both settings")
	}
	if (Config{TelegramChatID: "42"}).AlertsEnabled() || (Config{TelegramBotToken: "x"}).AlertsEnabled() {
		t.Error("alerts on with one setting missing")
	}
}
