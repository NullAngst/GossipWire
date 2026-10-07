package config

import (
	"os"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
}

func base() map[string]string {
	return map[string]string{
		"OWNER_ID":           "234567890123456789",
		"FEDERATION_MODE":    "off",
		"DISCORD_TOKEN_FILE": "/run/secrets/discord_token",
	}
}

var tokenFiles = map[string]string{"/run/secrets/discord_token": "abc.def.ghi\n"}

func TestLoadSetups(t *testing.T) {
	cases := []struct {
		mode, url, want string
	}{
		{"off", "", SetupA},
		{"allowlist", "", SetupB},
		{"allowlist", "https://alerts.mydomain.net", SetupC},
		{"open", "https://alerts.mydomain.net", SetupC},
	}
	for _, c := range cases {
		m := base()
		m["FEDERATION_MODE"] = c.mode
		m["INSTANCE_URL"] = c.url
		cfg, _, err := Load(env(m), files(tokenFiles))
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if cfg.Setup() != c.want {
			t.Errorf("%v: got setup %s", c, cfg.Setup())
		}
		if cfg.Token != "abc.def.ghi" {
			t.Errorf("token not trimmed: %q", cfg.Token)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name   string
		change map[string]string
		files  map[string]string
		want   string
	}{
		{"missing mode", map[string]string{"FEDERATION_MODE": ""}, tokenFiles, "FEDERATION_MODE is missing"},
		{"bad mode", map[string]string{"FEDERATION_MODE": "yes"}, tokenFiles, "not a valid choice"},
		{"owner username", map[string]string{"OWNER_ID": "zack"}, tokenFiles, `OWNER_ID "zack" is not a Discord user ID`},
		{"owner placeholder", map[string]string{"OWNER_ID": PlaceholderOwnerID}, tokenFiles, "still the example value"},
		{"open without url", map[string]string{"FEDERATION_MODE": "open"}, tokenFiles, "Open mode needs other instances to reach you"},
		{"plain http", map[string]string{"FEDERATION_MODE": "allowlist", "INSTANCE_URL": "http://alerts.mydomain.net"}, tokenFiles, "must start with https://"},
		{"example url", map[string]string{"FEDERATION_MODE": "allowlist", "INSTANCE_URL": "https://alerts.example.org"}, tokenFiles, "still an example address"},
		{"url path", map[string]string{"FEDERATION_MODE": "allowlist", "INSTANCE_URL": "https://a.mydomain.net/bot"}, tokenFiles, "extra parts"},
		{"no token", nil, map[string]string{}, "No bot token found"},
		{"empty token", nil, map[string]string{"/run/secrets/discord_token": "  \n"}, "empty"},
		{"both tokens", map[string]string{"DISCORD_TOKEN": "x.y.z"}, tokenFiles, "Both DISCORD_TOKEN_FILE and DISCORD_TOKEN"},
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, tokenFiles, "LOG_LEVEL"},
	}
	for _, c := range cases {
		m := base()
		for k, v := range c.change {
			m[k] = v
		}
		_, _, err := Load(env(m), files(c.files))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestWarnings(t *testing.T) {
	m := base()
	m["INSTANCE_URL"] = "https://alerts.mydomain.net"
	cfg, warns, err := Load(env(m), files(tokenFiles))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceURL != "" || len(warns) != 1 || !strings.Contains(warns[0], "so it is ignored") {
		t.Fatalf("expected the ignored-URL warning, got %v %q", warns, cfg.InstanceURL)
	}
	m["FEDERATION_MODE"] = "allowlist"
	m["INSTANCE_URL"] = "https://Alerts.MyDomain.net/"
	cfg, warns, err = Load(env(m), files(tokenFiles))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceURL != "https://alerts.mydomain.net" || cfg.InstanceHost != "alerts.mydomain.net" || len(warns) != 1 {
		t.Fatalf("got %q %q %v", cfg.InstanceURL, cfg.InstanceHost, warns)
	}
}

func TestEnvToken(t *testing.T) {
	m := base()
	delete(m, "DISCORD_TOKEN_FILE")
	m["DISCORD_TOKEN"] = "Bot a.b.c"
	cfg, _, err := Load(env(m), files(nil))
	if err != nil || cfg.Token != "a.b.c" {
		t.Fatalf("got %v %q", err, cfg.Token)
	}
}

func TestCheckDataDir(t *testing.T) {
	if err := CheckDataDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if err := CheckDataDir(dir); err == nil || !strings.Contains(err.Error(), "chown 65532:65532") {
		t.Fatalf("got %v", err)
	}
}
