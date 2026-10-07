// Package config reads GossipWire's settings from the environment and checks
// them before anything connects to Discord. Every error is written for a
// person who has never read the code: what is wrong, which line to fix, and
// what a correct value looks like.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Setup letters, matching the README's "Pick your setup" table.
const (
	SetupA = "A"
	SetupB = "B"
	SetupC = "C"
)

// Federation modes.
const (
	ModeOff       = "off"
	ModeAllowlist = "allowlist"
	ModeOpen      = "open"
)

// Placeholder values from the shipped compose file and README. The startup
// checks reject these so nobody runs with a copied example.
const (
	PlaceholderOwnerID = "123456789012345678"
	placeholderDomain  = "example.org"
)

// Config is everything set in the compose file.
type Config struct {
	OwnerID        string
	FederationMode string
	InstanceURL    string // empty, or https://host[:port] with no trailing slash
	InstanceHost   string // host[:port] from InstanceURL
	Token          string
	FedListen      string
	LogLevel       slog.Level
	DataDir        string
}

// Setup returns A, B or C, the same words the README and /instance status use.
func (c *Config) Setup() string {
	switch {
	case c.FederationMode == ModeOff:
		return SetupA
	case c.InstanceURL == "":
		return SetupB
	default:
		return SetupC
	}
}

// SetupDescription is the plain-words name of the running setup.
func (c *Config) SetupDescription() string {
	switch c.Setup() {
	case SetupA:
		return "Setup A: just my servers"
	case SetupB:
		return "Setup B: home connection, talk to others"
	default:
		return "Setup C: public, talk to others"
	}
}

// Reachable reports whether other instances can reach this one directly.
func (c *Config) Reachable() bool {
	return c.FederationMode != ModeOff && c.InstanceURL != ""
}

var snowflakeRe = regexp.MustCompile(`^[0-9]{17,20}$`)

// IsSnowflake reports whether s looks like a Discord ID.
func IsSnowflake(s string) bool { return snowflakeRe.MatchString(s) }

// Load reads and checks the settings. getenv is os.Getenv in production and a
// map lookup in tests. readFile is os.ReadFile in production. The returned
// warnings do not stop the bot and should be printed once.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, []string, error) {
	var warnings []string
	c := &Config{}

	// OWNER_ID
	c.OwnerID = strings.TrimSpace(getenv("OWNER_ID"))
	switch {
	case c.OwnerID == "":
		return nil, nil, errors.New("OWNER_ID is missing. Set it to your Discord user ID in the compose file. " +
			"Turn on Developer Mode in Discord (Settings > Advanced), right-click your name, and pick Copy User ID.")
	case c.OwnerID == PlaceholderOwnerID:
		return nil, nil, errors.New("OWNER_ID is still the example value 123456789012345678. Replace it with your own Discord user ID. " +
			"Turn on Developer Mode in Discord, right-click your name, and pick Copy User ID.")
	case !IsSnowflake(c.OwnerID):
		return nil, nil, fmt.Errorf("OWNER_ID %q is not a Discord user ID. It should be a long number like 123456789012345678. "+
			"Turn on Developer Mode in Discord, right-click your name, and pick Copy User ID.", c.OwnerID)
	}

	// FEDERATION_MODE
	c.FederationMode = strings.ToLower(strings.TrimSpace(getenv("FEDERATION_MODE")))
	switch c.FederationMode {
	case "":
		return nil, nil, errors.New("FEDERATION_MODE is missing. Set it to off, allowlist or open in the compose file. If you are not sure, use off.")
	case ModeOff, ModeAllowlist, ModeOpen:
	default:
		return nil, nil, fmt.Errorf("FEDERATION_MODE %q is not a valid choice. Set it to off, allowlist or open in the compose file. If you are not sure, use off.", c.FederationMode)
	}

	// INSTANCE_URL
	raw := strings.TrimSpace(getenv("INSTANCE_URL"))
	if raw != "" {
		if strings.HasSuffix(raw, "/") {
			raw = strings.TrimRight(raw, "/")
			warnings = append(warnings, "INSTANCE_URL ended with a slash. I removed it. Leave the slash off in the compose file.")
		}
		if !strings.HasPrefix(strings.ToLower(raw), "https://") {
			return nil, nil, errors.New("INSTANCE_URL must start with https://. Other instances will not talk to plain http. Example: https://alerts.example.org")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Hostname() == "" {
			return nil, nil, fmt.Errorf("INSTANCE_URL %q is not a web address. It should look like https://alerts.example.org", raw)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, nil, fmt.Errorf("INSTANCE_URL %q has extra parts after the address. Use only the scheme and host, like https://alerts.example.org", raw)
		}
		h := strings.ToLower(u.Hostname())
		if h == placeholderDomain || strings.HasSuffix(h, "."+placeholderDomain) || h == "example.com" || strings.HasSuffix(h, ".example.com") {
			return nil, nil, fmt.Errorf("INSTANCE_URL is still an example address (%s). Put your own public https address there, or leave it empty (\"\") for setup A or B.", raw)
		}
		c.InstanceURL = "https://" + strings.ToLower(u.Host)
		c.InstanceHost = strings.ToLower(u.Host)
	}
	if c.FederationMode == ModeOpen && c.InstanceURL == "" {
		return nil, nil, errors.New("FEDERATION_MODE is open, but INSTANCE_URL is empty. Open mode needs other instances to reach you. Set INSTANCE_URL, or use allowlist instead.")
	}
	if c.FederationMode == ModeOff && c.InstanceURL != "" {
		warnings = append(warnings, "INSTANCE_URL is set but FEDERATION_MODE is off, so it is ignored.")
		c.InstanceURL = ""
		c.InstanceHost = ""
	}

	// Token
	tokenFile := strings.TrimSpace(getenv("DISCORD_TOKEN_FILE"))
	tokenEnv := strings.TrimSpace(getenv("DISCORD_TOKEN"))
	switch {
	case tokenFile != "" && tokenEnv != "":
		return nil, nil, errors.New("Both DISCORD_TOKEN_FILE and DISCORD_TOKEN are set. Use one. Remove the DISCORD_TOKEN line to keep using discord_token.txt, " +
			"or remove the DISCORD_TOKEN_FILE line and the secrets block to use the environment variable.")
	case tokenEnv != "":
		c.Token = tokenEnv
	case tokenFile != "":
		b, err := readFile(tokenFile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, errors.New("No bot token found. Put your token in discord_token.txt next to the compose file.")
			}
			if errors.Is(err, os.ErrPermission) {
				return nil, nil, errors.New("The bot cannot read discord_token.txt. Run: sudo chown 65532:65532 discord_token.txt (next to the compose file), " +
					"or podman unshare chown 65532:65532 discord_token.txt on rootless Podman.")
			}
			// A missing secret file often shows up as a directory, since
			// Docker creates the mount point when the source is missing.
			return nil, nil, errors.New("No bot token found. Put your token in discord_token.txt next to the compose file.")
		}
		c.Token = strings.TrimSpace(string(b))
	default:
		return nil, nil, errors.New("No bot token found. Put your token in discord_token.txt next to the compose file.")
	}
	if c.Token == "" {
		return nil, nil, errors.New("No bot token found. Put your token in discord_token.txt next to the compose file. The file exists but is empty.")
	}
	c.Token = strings.TrimPrefix(c.Token, "Bot ")
	if strings.ContainsAny(c.Token, " \t\r\n") {
		return nil, nil, errors.New("The bot token has spaces or more than one line in it. discord_token.txt should hold only the token, on one line.")
	}

	// FED_LISTEN
	c.FedListen = strings.TrimSpace(getenv("FED_LISTEN"))
	if c.FedListen == "" {
		c.FedListen = "0.0.0.0:8081"
	}

	// LOG_LEVEL
	switch strings.ToLower(strings.TrimSpace(getenv("LOG_LEVEL"))) {
	case "", "info":
		c.LogLevel = slog.LevelInfo
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return nil, nil, fmt.Errorf("LOG_LEVEL %q is not a valid choice. Use debug, info, warn or error, or remove the line to use info.", getenv("LOG_LEVEL"))
	}

	c.DataDir = strings.TrimSpace(getenv("DATA_DIR"))
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	return c, warnings, nil
}

// CheckDataDir makes sure the bot can write to its data folder.
func CheckDataDir(dir string) error {
	msg := fmt.Errorf("Cannot write to %s. Run: chown 65532:65532 ./data (or add :U to the volume line on rootless Podman).", dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return msg
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return msg
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
